package connector

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"maunium.net/go/mautrix-whatsapp/pkg/connector/wadb"
	"maunium.net/go/mautrix-whatsapp/pkg/waid"
)

var _ bridgev2.BackfillingNetworkAPI = (*WhatsAppClient)(nil)

const historySyncDispatchWait = 30 * time.Second

func (wa *WhatsAppClient) historySyncLoop(ctx context.Context) {
	dispatchTimer := time.NewTimer(historySyncDispatchWait)
	dispatchTimer.Stop()
	wa.UserLogin.Log.Debug().Msg("Starting history sync loop")
	for {
		select {
		case evt := <-wa.historySyncs:
			dispatchTimer.Stop()
			wa.handleWAHistorySync(ctx, evt)
			dispatchTimer.Reset(historySyncDispatchWait)
		case <-dispatchTimer.C:
			wa.createPortalsFromHistorySync(ctx)
		case <-ctx.Done():
			wa.UserLogin.Log.Debug().Msg("Stopping history sync loop")
			return
		}
	}
}

func (wa *WhatsAppClient) handleWAHistorySync(ctx context.Context, evt *waHistorySync.HistorySync) {
	if evt == nil || evt.SyncType == nil {
		return
	}
	log := wa.UserLogin.Log.With().
		Str("action", "store history sync").
		Stringer("sync_type", evt.GetSyncType()).
		Uint32("chunk_order", evt.GetChunkOrder()).
		Uint32("progress", evt.GetProgress()).
		Logger()
	ctx = log.WithContext(ctx)
	if evt.GetGlobalSettings() != nil {
		log.Debug().Interface("global_settings", evt.GetGlobalSettings()).Msg("Got global settings in history sync")
	}
	if evt.GetSyncType() == waHistorySync.HistorySync_INITIAL_STATUS_V3 || evt.GetSyncType() == waHistorySync.HistorySync_PUSH_NAME || evt.GetSyncType() == waHistorySync.HistorySync_NON_BLOCKING_DATA {
		log.Debug().
			Int("conversation_count", len(evt.GetConversations())).
			Int("pushname_count", len(evt.GetPushnames())).
			Int("status_count", len(evt.GetStatusV3Messages())).
			Int("recent_sticker_count", len(evt.GetRecentStickers())).
			Int("past_participant_count", len(evt.GetPastParticipants())).
			Msg("Ignoring history sync")
		return
	}
	log.Info().
		Int("conversation_count", len(evt.GetConversations())).
		Int("past_participant_count", len(evt.GetPastParticipants())).
		Msg("Storing history sync")
	successfullySavedTotal := 0
	failedToSaveTotal := 0
	totalMessageCount := 0
	for _, conv := range evt.GetConversations() {
		jid, err := types.ParseJID(conv.GetID())
		if err != nil {
			totalMessageCount += len(conv.GetMessages())
			log.Warn().Err(err).
				Str("chat_jid", conv.GetID()).
				Int("msg_count", len(conv.GetMessages())).
				Msg("Failed to parse chat JID in history sync")
			continue
		} else if jid.Server == types.BroadcastServer {
			log.Debug().Str("chat_jid", jid.String()).Msg("Skipping broadcast list in history sync")
			continue
		} else if jid.Server == types.HiddenUserServer {
			log.Debug().Str("chat_jid", jid.String()).Msg("Skipping hidden user JID chat in history sync")
			continue
		}
		totalMessageCount += len(conv.GetMessages())
		log := log.With().
			Stringer("chat_jid", jid).
			Int("msg_count", len(conv.GetMessages())).
			Logger()

		var minTime, maxTime time.Time
		var minTimeIndex, maxTimeIndex int

		ignoredTypes := 0
		messages := make([]*wadb.HistorySyncMessageTuple, 0, len(conv.GetMessages()))
		for i, rawMsg := range conv.GetMessages() {
			// Don't store messages that will just be skipped.
			msgEvt, err := wa.Client.ParseWebMessage(jid, rawMsg.GetMessage())
			if err != nil {
				log.Warn().Err(err).
					Int("msg_index", i).
					Str("msg_id", rawMsg.GetMessage().GetKey().GetID()).
					Uint64("msg_time_seconds", rawMsg.GetMessage().GetMessageTimestamp()).
					Msg("Dropping historical message due to parse error")
				continue
			}
			if minTime.IsZero() || msgEvt.Info.Timestamp.Before(minTime) {
				minTime = msgEvt.Info.Timestamp
				minTimeIndex = i
			}
			if maxTime.IsZero() || msgEvt.Info.Timestamp.After(maxTime) {
				maxTime = msgEvt.Info.Timestamp
				maxTimeIndex = i
			}

			msgType := getMessageType(msgEvt.Message)
			if msgType == "ignore" || strings.HasPrefix(msgType, "unknown_protocol_") {
				ignoredTypes++
				continue
			}
			marshaled, err := proto.Marshal(rawMsg)
			if err != nil {
				log.Warn().Err(err).
					Int("msg_index", i).
					Str("msg_id", msgEvt.Info.ID).
					Msg("Failed to marshal message")
				continue
			}

			messages = append(messages, &wadb.HistorySyncMessageTuple{Info: &msgEvt.Info, Message: marshaled})
		}
		log.Debug().
			Int("wrapped_count", len(messages)).
			Int("ignored_msg_type_count", ignoredTypes).
			Time("lowest_time", minTime).
			Int("lowest_time_index", minTimeIndex).
			Time("highest_time", maxTime).
			Int("highest_time_index", maxTimeIndex).
			Dict("metadata", zerolog.Dict().
				Uint32("ephemeral_expiration", conv.GetEphemeralExpiration()).
				Int64("ephemeral_setting_timestamp", conv.GetEphemeralSettingTimestamp()).
				Bool("marked_unread", conv.GetMarkedAsUnread()).
				Bool("archived", conv.GetArchived()).
				Uint32("pinned", conv.GetPinned()).
				Uint64("mute_end", conv.GetMuteEndTime()).
				Uint32("unread_count", conv.GetUnreadCount()),
			).
			Msg("Collected messages to save from history sync conversation")

		if len(messages) > 0 {
			err = wa.Main.DB.Conversation.Put(ctx, wadb.NewConversation(wa.UserLogin.ID, jid, conv))
			if err != nil {
				log.Err(err).Msg("Failed to save conversation metadata")
				continue
			}
			err = wa.Main.DB.Message.Put(ctx, wa.UserLogin.ID, jid, messages)
			if err != nil {
				log.Err(err).Msg("Failed to save messages")
				failedToSaveTotal += len(messages)
			} else {
				successfullySavedTotal += len(messages)
			}
		}
	}
	log.Info().
		Int("total_saved_count", successfullySavedTotal).
		Int("total_failed_count", failedToSaveTotal).
		Int("total_message_count", totalMessageCount).
		Msg("Finished storing history sync")
}

func (wa *WhatsAppClient) createPortalsFromHistorySync(ctx context.Context) {
	log := wa.UserLogin.Log.With().
		Str("action", "create portals from history sync").
		Logger()
	ctx = log.WithContext(ctx)
	limit := wa.Main.Config.HistorySync.MaxInitialConversations
	log.Info().Int("limit", limit).Msg("Creating portals from history sync")
	conversations, err := wa.Main.DB.Conversation.GetRecent(ctx, wa.UserLogin.ID, limit)
	if err != nil {
		log.Err(err).Msg("Failed to get recent conversations from database")
		return
	}
	for _, conv := range conversations {
		wrappedInfo, err := wa.getChatInfo(ctx, conv.ChatJID, conv)
		if errors.Is(err, whatsmeow.ErrNotInGroup) {
			log.Debug().Stringer("chat_jid", conv.ChatJID).
				Msg("Skipping creating room because the user is not a participant")
			err = wa.Main.DB.Message.DeleteAllInChat(ctx, wa.UserLogin.ID, conv.ChatJID)
			if err != nil {
				log.Err(err).Msg("Failed to delete historical messages for portal")
			}
			continue
		} else if err != nil {
			log.Err(err).Stringer("chat_jid", conv.ChatJID).Msg("Failed to get chat info")
			continue
		}
		wa.Main.Bridge.QueueRemoteEvent(wa.UserLogin, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				LogContext:   nil,
				PortalKey:    wa.makeWAPortalKey(conv.ChatJID),
				CreatePortal: true,
			},
			ChatInfo:        wrappedInfo,
			LatestMessageTS: conv.LastMessageTimestamp,
		})
	}
}

func (wa *WhatsAppClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	portalJID, err := waid.ParsePortalID(params.Portal.ID)
	if err != nil {
		return nil, err
	}
	var markRead bool
	var startTime, endTime *time.Time
	if params.Forward {
		if params.AnchorMessage != nil {
			startTime = ptr.Ptr(params.AnchorMessage.Timestamp)
		}
		conv, err := wa.Main.DB.Conversation.Get(ctx, wa.UserLogin.ID, portalJID)
		if err != nil {
			return nil, fmt.Errorf("failed to get conversation from database: %w", err)
		} else if conv != nil {
			markRead = !ptr.Val(conv.MarkedAsUnread) && ptr.Val(conv.UnreadCount) == 0
		}
	} else if params.Cursor != "" {
		endTimeUnix, err := strconv.ParseInt(string(params.Cursor), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse cursor: %w", err)
		}
		endTime = ptr.Ptr(time.Unix(endTimeUnix, 0))
	} else if params.AnchorMessage != nil {
		endTime = ptr.Ptr(params.AnchorMessage.Timestamp)
	}
	messages, err := wa.Main.DB.Message.GetBetween(ctx, wa.UserLogin.ID, portalJID, startTime, endTime, params.Count+1)
	if err != nil {
		return nil, fmt.Errorf("failed to load messages from database: %w", err)
	} else if len(messages) == 0 {
		return &bridgev2.FetchMessagesResponse{
			HasMore: false,
			Forward: params.Forward,
		}, nil
	}
	hasMore := false
	oldestTS := messages[len(messages)-1].GetMessageTimestamp()
	newestTS := messages[0].GetMessageTimestamp()
	if len(messages) > params.Count {
		hasMore = true
		// For safety, cut off messages with the oldest timestamp in the response.
		// Otherwise, if there are multiple messages with the same timestamp, the next fetch may miss some.
		for i := len(messages) - 2; i >= 0; i-- {
			if messages[i].GetMessageTimestamp() > oldestTS {
				messages = messages[:i+1]
				break
			}
		}
	}
	convertedMessages := make([]*bridgev2.BackfillMessage, len(messages))
	for i, msg := range messages {
		evt, err := wa.Client.ParseWebMessage(portalJID, msg)
		if err != nil {
			// This should never happen because the info is already parsed once before being stored in the database
			return nil, fmt.Errorf("failed to parse info of message %s: %w", msg.GetKey().GetID(), err)
		}
		convertedMessages[i] = wa.convertHistorySyncMessage(ctx, params.Portal, &evt.Info, msg)
	}
	return &bridgev2.FetchMessagesResponse{
		Messages: convertedMessages,
		Cursor:   networkid.PaginationCursor(strconv.FormatUint(messages[0].GetMessageTimestamp(), 10)),
		HasMore:  hasMore,
		Forward:  endTime == nil,
		MarkRead: markRead,
		// TODO set remaining or total count
		CompleteCallback: func() {
			// TODO this only deletes after backfilling. If there's no need for backfill after a relogin,
			//      the messages will be stuck in the database
			var err error
			if !wa.Main.Bridge.Config.Backfill.Queue.Enabled {
				// If the backfill queue isn't enabled, delete all messages after backfilling a batch.
				err = wa.Main.DB.Message.DeleteAllInChat(ctx, wa.UserLogin.ID, portalJID)
			} else {
				// Otherwise just delete the messages that got backfilled
				err = wa.Main.DB.Message.DeleteBetween(ctx, wa.UserLogin.ID, portalJID, newestTS, oldestTS)
			}
			if err != nil {
				zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to delete messages from database after backfill")
			}
		},
	}, nil
}

func (wa *WhatsAppClient) convertHistorySyncMessage(
	ctx context.Context, portal *bridgev2.Portal, info *types.MessageInfo, msg *waWeb.WebMessageInfo,
) *bridgev2.BackfillMessage {
	// TODO use proper intent
	intent := wa.Main.Bridge.Bot
	wrapped := &bridgev2.BackfillMessage{
		ConvertedMessage: wa.Main.MsgConv.ToMatrix(ctx, portal, wa.Client, intent, msg.Message, info),
		Sender:           wa.makeEventSender(info.Sender),
		ID:               waid.MakeMessageID(info.Chat, info.Sender, info.ID),
		TxnID:            networkid.TransactionID(waid.MakeMessageID(info.Chat, info.Sender, info.ID)),
		Timestamp:        info.Timestamp,
		Reactions:        make([]*bridgev2.BackfillReaction, len(msg.Reactions)),
	}
	for i, reaction := range msg.Reactions {
		var sender types.JID
		if reaction.GetKey().GetFromMe() {
			sender = wa.JID
		} else if reaction.GetKey().GetParticipant() != "" {
			sender, _ = types.ParseJID(*reaction.Key.Participant)
		} else if info.Chat.Server == types.DefaultUserServer {
			sender = info.Chat
		}
		if sender.IsEmpty() {
			continue
		}
		wrapped.Reactions[i] = &bridgev2.BackfillReaction{
			TargetPart: ptr.Ptr(networkid.PartID("")),
			Timestamp:  time.UnixMilli(reaction.GetSenderTimestampMS()),
			Sender:     wa.makeEventSender(sender),
			Emoji:      reaction.GetText(),
		}
	}
	return wrapped
}
