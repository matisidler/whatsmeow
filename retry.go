// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"go.mau.fi/libsignal/ecc"
	"go.mau.fi/libsignal/groups"
	"go.mau.fi/libsignal/keys/prekey"
	"go.mau.fi/libsignal/protocol"
	"google.golang.org/protobuf/proto"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	"go.mau.fi/whatsmeow/proto/waMsgTransport"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Number of sent messages to cache in memory for handling retry receipts.
const recentMessagesSize = 256

type recentMessageKey struct {
	To types.JID
	ID types.MessageID
}

type RecentMessage struct {
	wa *waE2E.Message
	fb *waMsgApplication.MessageApplication
}

func (rm RecentMessage) IsEmpty() bool {
	return rm.wa == nil && rm.fb == nil
}

func (cli *Client) addRecentMessage(to types.JID, id types.MessageID, wa *waE2E.Message, fb *waMsgApplication.MessageApplication) {
	cli.recentMessagesLock.Lock()
	key := recentMessageKey{to, id}
	if cli.recentMessagesList[cli.recentMessagesPtr].ID != "" {
		delete(cli.recentMessagesMap, cli.recentMessagesList[cli.recentMessagesPtr])
	}
	cli.recentMessagesMap[key] = RecentMessage{wa: wa, fb: fb}
	cli.recentMessagesList[cli.recentMessagesPtr] = key
	cli.recentMessagesPtr++
	if cli.recentMessagesPtr >= len(cli.recentMessagesList) {
		cli.recentMessagesPtr = 0
	}
	cli.recentMessagesLock.Unlock()
}

func (cli *Client) getRecentMessage(to types.JID, id types.MessageID) RecentMessage {
	cli.recentMessagesLock.RLock()
	msg, _ := cli.recentMessagesMap[recentMessageKey{to, id}]
	cli.recentMessagesLock.RUnlock()
	return msg
}

func (cli *Client) getMessageForRetry(ctx context.Context, receipt *events.Receipt, messageID types.MessageID) (RecentMessage, error) {
	msg := cli.getRecentMessage(receipt.Chat, messageID)
	if msg.IsEmpty() {
		waMsg := cli.GetMessageForRetry(receipt.Sender, receipt.Chat, messageID)
		if waMsg == nil {
			return RecentMessage{}, fmt.Errorf("couldn't find message %s", messageID)
		} else {
			fmt.Printf("DEBUG GREETINGS: Found message in GetMessageForRetry to accept retry receipt for %s/%s from %s\n", receipt.Chat, messageID, receipt.Sender)
		}
		msg = RecentMessage{wa: waMsg}
	} else {
		fmt.Printf("DEBUG GREETINGS: Found message in local cache to accept retry receipt for %s/%s from %s\n", receipt.Chat, messageID, receipt.Sender)
	}
	return msg, nil
}

const recreateSessionTimeout = 1 * time.Hour
const recreateSessionTimeoutAggressive = 5 * time.Minute // Shorter timeout for automated greeting scenarios

func (cli *Client) shouldRecreateSession(ctx context.Context, retryCount int, jid types.JID) (reason string, recreate bool) {
	cli.sessionRecreateHistoryLock.Lock()
	defer cli.sessionRecreateHistoryLock.Unlock()
	if contains, err := cli.Store.ContainsSession(ctx, jid.SignalAddress()); err != nil {
		return "", false
	} else if !contains {
		cli.sessionRecreateHistory[jid] = time.Now()
		return "we don't have a Signal session with them", true
	} else if retryCount < 2 {
		return "", false
	}

	// Use shorter timeout for bot accounts or potential automated greeting scenarios
	timeout := recreateSessionTimeout
	if jid.IsBot() {
		timeout = recreateSessionTimeoutAggressive
	}

	prevTime, ok := cli.sessionRecreateHistory[jid]
	if !ok || prevTime.Add(timeout).Before(time.Now()) {
		cli.sessionRecreateHistory[jid] = time.Now()
		if timeout == recreateSessionTimeoutAggressive {
			return fmt.Sprintf("retry count > 1 and over %v since last recreation (aggressive mode for bot/automated)", timeout), true
		}
		return "retry count > 1 and over an hour since last recreation", true
	}
	return "", false
}

type incomingRetryKey struct {
	jid       types.JID
	messageID types.MessageID
}

// handleRetryReceipt handles an incoming retry receipt for an outgoing message.
func (cli *Client) handleRetryReceipt(ctx context.Context, receipt *events.Receipt, node *waBinary.Node) error {
	retryChild, ok := node.GetOptionalChildByTag("retry")
	if !ok {
		return &ElementMissingError{Tag: "retry", In: "retry receipt"}
	}
	ag := retryChild.AttrGetter()
	messageID := ag.String("id")
	timestamp := ag.UnixTime("t")
	retryCount := ag.Int("count")
	if !ag.OK() {
		return ag.Error()
	}
	msg, err := cli.getMessageForRetry(ctx, receipt, messageID)
	if err != nil {
		return err
	}
	var fbConsumerMsg *waConsumerApplication.ConsumerApplication
	if msg.fb != nil {
		subProto, ok := msg.fb.GetPayload().GetSubProtocol().GetSubProtocol().(*waMsgApplication.MessageApplication_SubProtocolPayload_ConsumerMessage)
		if ok {
			fbConsumerMsg, err = subProto.Decode()
			if err != nil {
				return fmt.Errorf("failed to decode consumer message for retry: %w", err)
			}
		}
	}

	retryKey := incomingRetryKey{receipt.Sender, messageID}
	cli.incomingRetryRequestCounterLock.Lock()
	cli.incomingRetryRequestCounter[retryKey]++
	internalCounter := cli.incomingRetryRequestCounter[retryKey]
	cli.incomingRetryRequestCounterLock.Unlock()
	if internalCounter >= 10 {
		fmt.Printf("DEBUG GREETINGS: Dropping retry request from %s for %s: internal retry counter is %d\n", messageID, receipt.Sender, internalCounter)
		return nil
	}

	var fbSKDM *waMsgTransport.MessageTransport_Protocol_Ancillary_SenderKeyDistributionMessage
	var fbDSM *waMsgTransport.MessageTransport_Protocol_Integral_DeviceSentMessage
	if receipt.IsGroup {
		builder := groups.NewGroupSessionBuilder(cli.Store, pbSerializer)
		senderKeyName := protocol.NewSenderKeyName(receipt.Chat.String(), cli.getOwnLID().SignalAddress())
		signalSKDMessage, err := builder.Create(ctx, senderKeyName)
		if err != nil {
			fmt.Printf("DEBUG GREETINGS: Failed to create sender key distribution message to include in retry of %s in %s to %s: %v\n", messageID, receipt.Chat, receipt.Sender, err)
		}
		if msg.wa != nil {
			msg.wa.SenderKeyDistributionMessage = &waE2E.SenderKeyDistributionMessage{
				GroupID:                             proto.String(receipt.Chat.String()),
				AxolotlSenderKeyDistributionMessage: signalSKDMessage.Serialize(),
			}
		} else {
			fbSKDM = &waMsgTransport.MessageTransport_Protocol_Ancillary_SenderKeyDistributionMessage{
				GroupID:                             proto.String(receipt.Chat.String()),
				AxolotlSenderKeyDistributionMessage: signalSKDMessage.Serialize(),
			}
		}
	} else if receipt.IsFromMe {
		if msg.wa != nil {
			msg.wa = &waE2E.Message{
				DeviceSentMessage: &waE2E.DeviceSentMessage{
					DestinationJID: proto.String(receipt.Chat.String()),
					Message:        msg.wa,
				},
			}
		} else {
			fbDSM = &waMsgTransport.MessageTransport_Protocol_Integral_DeviceSentMessage{
				DestinationJID: proto.String(receipt.Chat.String()),
			}
		}
	}

	// TODO pre-retry callback for fb
	if cli.PreRetryCallback != nil && !cli.PreRetryCallback(receipt, messageID, retryCount, msg.wa) {
		fmt.Printf("DEBUG GREETINGS: Cancelled retry receipt in PreRetryCallback\n")
		return nil
	}

	var plaintext, frankingTag []byte
	if msg.wa != nil {
		plaintext, err = proto.Marshal(msg.wa)
		if err != nil {
			return fmt.Errorf("failed to marshal message: %w", err)
		}
	} else {
		plaintext, err = proto.Marshal(msg.fb)
		if err != nil {
			return fmt.Errorf("failed to marshal consumer message: %w", err)
		}
		frankingHash := hmac.New(sha256.New, msg.fb.GetMetadata().GetFrankingKey())
		frankingHash.Write(plaintext)
		frankingTag = frankingHash.Sum(nil)
	}
	_, hasKeys := node.GetOptionalChildByTag("keys")
	var bundle *prekey.Bundle
	if hasKeys {
		bundle, err = nodeToPreKeyBundle(uint32(receipt.Sender.Device), *node)
		if err != nil {
			return fmt.Errorf("failed to read prekey bundle in retry receipt: %w", err)
		}
	} else if reason, recreate := cli.shouldRecreateSession(ctx, retryCount, receipt.Sender); recreate {
		fmt.Printf("DEBUG GREETINGS: Fetching prekeys for %s for handling retry receipt with no prekey bundle because %s\n", receipt.Sender, reason)
		var keys map[types.JID]preKeyResp
		keys, err = cli.fetchPreKeys(ctx, []types.JID{receipt.Sender})
		if err != nil {
			return err
		}
		bundle, err = keys[receipt.Sender].bundle, keys[receipt.Sender].err
		if err != nil {
			return fmt.Errorf("failed to fetch prekeys: %w", err)
		} else if bundle == nil {
			return fmt.Errorf("didn't get prekey bundle for %s (response size: %d)", receipt.Sender, len(keys))
		}
	}
	encAttrs := waBinary.Attrs{}
	var msgAttrs messageAttrs
	if msg.wa != nil {
		msgAttrs.MediaType = getMediaTypeFromMessage(msg.wa)
		msgAttrs.Type = getTypeFromMessage(msg.wa)
	} else if fbConsumerMsg != nil {
		msgAttrs = getAttrsFromFBMessage(fbConsumerMsg)
	} else {
		msgAttrs.Type = "text"
	}
	if msgAttrs.MediaType != "" {
		encAttrs["mediatype"] = msgAttrs.MediaType
	}
	var encrypted *waBinary.Node
	var includeDeviceIdentity bool
	if msg.wa != nil {
		encryptionIdentity := receipt.Sender
		if receipt.Sender.Server == types.DefaultUserServer {
			lidForPN, err := cli.Store.LIDs.GetLIDForPN(ctx, receipt.Sender)
			if err != nil {
				fmt.Printf("DEBUG GREETINGS: Failed to get LID for %s: %v\n", receipt.Sender, err)
			} else if !lidForPN.IsEmpty() {
				cli.migrateSessionStore(ctx, receipt.Sender, lidForPN)
				encryptionIdentity = lidForPN
			}
		}
		encrypted, includeDeviceIdentity, err = cli.encryptMessageForDevice(ctx, plaintext, encryptionIdentity, bundle, encAttrs)
	} else {
		encrypted, err = cli.encryptMessageForDeviceV3(ctx, &waMsgTransport.MessageTransport_Payload{
			ApplicationPayload: &waCommon.SubProtocol{
				Payload: plaintext,
				Version: proto.Int32(FBMessageApplicationVersion),
			},
			FutureProof: waCommon.FutureProofBehavior_PLACEHOLDER.Enum(),
		}, fbSKDM, fbDSM, receipt.Sender, bundle, encAttrs)
	}
	if err != nil {
		return fmt.Errorf("failed to encrypt message for retry: %w", err)
	}
	encrypted.Attrs["count"] = retryCount

	attrs := waBinary.Attrs{
		"to":   node.Attrs["from"],
		"type": msgAttrs.Type,
		"id":   messageID,
		"t":    timestamp.Unix(),
	}
	if !receipt.IsGroup {
		attrs["device_fanout"] = false
	}
	if participant, ok := node.Attrs["participant"]; ok {
		attrs["participant"] = participant
	}
	if recipient, ok := node.Attrs["recipient"]; ok {
		attrs["recipient"] = recipient
	}
	if edit, ok := node.Attrs["edit"]; ok {
		attrs["edit"] = edit
	}
	var content []waBinary.Node
	if msg.wa != nil {
		content = cli.getMessageContent(
			*encrypted, msg.wa, attrs, includeDeviceIdentity, nodeExtraParams{},
		)
	} else {
		content = []waBinary.Node{
			*encrypted,
			{Tag: "franking", Content: []waBinary.Node{{Tag: "franking_tag", Content: frankingTag}}},
		}
	}
	err = cli.sendNode(waBinary.Node{
		Tag:     "message",
		Attrs:   attrs,
		Content: content,
	})
	if err != nil {
		return fmt.Errorf("failed to send retry message: %w", err)
	}
	fmt.Printf("DEBUG GREETINGS: Sent retry #%d for %s/%s to %s\n", retryCount, receipt.Chat, messageID, receipt.Sender)
	return nil
}

func (cli *Client) cancelDelayedRequestFromPhone(msgID types.MessageID) {
	if !cli.AutomaticMessageRerequestFromPhone || cli.MessengerConfig != nil {
		return
	}
	cli.pendingPhoneRerequestsLock.RLock()
	cancelPendingRequest, ok := cli.pendingPhoneRerequests[msgID]
	if ok {
		cancelPendingRequest()
	}
	cli.pendingPhoneRerequestsLock.RUnlock()
}

// RequestFromPhoneDelay specifies how long to wait for the sender to resend the message before requesting from your phone.
// This is only used if Client.AutomaticMessageRerequestFromPhone is true.
var RequestFromPhoneDelay = 5 * time.Second

func (cli *Client) delayedRequestMessageFromPhone(info *types.MessageInfo) {
	if !cli.AutomaticMessageRerequestFromPhone || cli.MessengerConfig != nil {
		return
	}
	cli.pendingPhoneRerequestsLock.Lock()
	_, alreadyRequesting := cli.pendingPhoneRerequests[info.ID]
	if alreadyRequesting {
		cli.pendingPhoneRerequestsLock.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cli.pendingPhoneRerequests[info.ID] = cancel
	cli.pendingPhoneRerequestsLock.Unlock()

	defer func() {
		cli.pendingPhoneRerequestsLock.Lock()
		delete(cli.pendingPhoneRerequests, info.ID)
		cli.pendingPhoneRerequestsLock.Unlock()
	}()
	select {
	case <-time.After(RequestFromPhoneDelay):
	case <-ctx.Done():
		fmt.Printf("DEBUG GREETINGS: Cancelled delayed request for message %s from phone\n", info.ID)
		return
	}
	_, err := cli.SendMessage(
		ctx,
		cli.getOwnID().ToNonAD(),
		cli.BuildUnavailableMessageRequest(info.Chat, info.Sender, info.ID),
		SendRequestExtra{Peer: true},
	)
	if err != nil {
		fmt.Printf("DEBUG GREETINGS: Failed to send request for unavailable message %s to phone: %v\n", info.ID, err)
	} else {
		fmt.Printf("DEBUG GREETINGS: Requested message %s from phone\n", info.ID)
	}
	return
}

func (cli *Client) clearDelayedMessageRequests() {
	cli.pendingPhoneRerequestsLock.Lock()
	defer cli.pendingPhoneRerequestsLock.Unlock()
	for _, cancel := range cli.pendingPhoneRerequests {
		cancel()
	}
}

// sendRetryReceipt sends a retry receipt for an incoming message.
func (cli *Client) sendRetryReceipt(ctx context.Context, node *waBinary.Node, info *types.MessageInfo, forceIncludeIdentity bool) {
	id, _ := node.Attrs["id"].(string)
	children := node.GetChildren()
	var retryCountInMsg int
	if len(children) == 1 && children[0].Tag == "enc" {
		retryCountInMsg = children[0].AttrGetter().OptionalInt("count")
	}

	cli.messageRetriesLock.Lock()
	cli.messageRetries[id]++
	retryCount := cli.messageRetries[id]
	// In case the message is a retry response, and we restarted in between, find the count from the message
	if retryCount == 1 && retryCountInMsg > 0 {
		retryCount = retryCountInMsg + 1
		cli.messageRetries[id] = retryCount
	}
	cli.messageRetriesLock.Unlock()

	// Increase retry limit for potential automated greeting scenarios
	maxRetries := 5
	// If this looks like it might be after an automated greeting (short time window, specific sender patterns)
	if cli.EnableEnhancedAutomatedGreetingRetry && cli.isLikelyPostAutomatedGreeting(info) {
		maxRetries = 8 // Allow more retries for these cases
		fmt.Printf("DEBUG GREETINGS: Detected potential post-automated-greeting message from %s, extending retry limit to %d\n", info.SourceString(), maxRetries)
	}

	if retryCount >= maxRetries {
		cli.Log.Warnf("Not sending any more retry receipts for %s (reached limit of %d)", id, maxRetries)
		return
	}
	if retryCount == 1 {
		go cli.delayedRequestMessageFromPhone(info)
	}

	// For automated greeting scenarios, be more aggressive about session recreation
	var shouldForceSessionRecreation bool
	if cli.EnableEnhancedAutomatedGreetingRetry && cli.isLikelyPostAutomatedGreeting(info) && retryCount >= 2 {
		shouldForceSessionRecreation = true
		fmt.Printf("DEBUG GREETINGS: Forcing session recreation for potential post-automated-greeting message from %s\n", info.SourceString())
	}

	var registrationIDBytes [4]byte
	binary.BigEndian.PutUint32(registrationIDBytes[:], cli.Store.RegistrationID)
	attrs := waBinary.Attrs{
		"id":   id,
		"type": "retry",
		"to":   node.Attrs["from"],
	}
	if recipient, ok := node.Attrs["recipient"]; ok {
		attrs["recipient"] = recipient
	}
	if participant, ok := node.Attrs["participant"]; ok {
		attrs["participant"] = participant
	}
	payload := waBinary.Node{
		Tag:   "receipt",
		Attrs: attrs,
		Content: []waBinary.Node{
			{Tag: "retry", Attrs: waBinary.Attrs{
				"count": retryCount,
				"id":    id,
				"t":     node.Attrs["t"],
				"v":     1,
			}},
			{Tag: "registration", Content: registrationIDBytes[:]},
		},
	}
	if retryCount > 1 || forceIncludeIdentity || shouldForceSessionRecreation {
		if key, err := cli.Store.PreKeys.GenOnePreKey(ctx); err != nil {
			cli.Log.Errorf("Failed to get prekey for retry receipt: %v", err)
		} else if deviceIdentity, err := proto.Marshal(cli.Store.Account); err != nil {
			cli.Log.Errorf("Failed to marshal account info: %v", err)
			return
		} else {
			payload.Content = append(payload.GetChildren(), waBinary.Node{
				Tag: "keys",
				Content: []waBinary.Node{
					{Tag: "type", Content: []byte{ecc.DjbType}},
					{Tag: "identity", Content: cli.Store.IdentityKey.Pub[:]},
					preKeyToNode(key),
					preKeyToNode(cli.Store.SignedPreKey),
					{Tag: "device-identity", Content: deviceIdentity},
				},
			})
		}
	}
	err := cli.sendNode(payload)
	if err != nil {
		cli.Log.Errorf("Failed to send retry receipt for %s: %v", id, err)
	}
}

// isLikelyPostAutomatedGreeting detects if a message might be following an automated greeting
func (cli *Client) isLikelyPostAutomatedGreeting(info *types.MessageInfo) bool {
	// Check if this is from a business account or has characteristics of automated greeting follow-up
	if info.Sender.IsBot() {
		return true
	}

	// Check our automated greeting tracker
	cli.automatedGreetingTrackerLock.RLock()
	lastAutomatedTime, hasAutomated := cli.automatedGreetingTracker[info.Sender]
	cli.automatedGreetingTrackerLock.RUnlock()

	if hasAutomated {
		// If we've seen an automated greeting from this sender recently (within 2 minutes)
		if time.Since(lastAutomatedTime) < 2*time.Minute {
			fmt.Printf("DEBUG GREETINGS: Detected message from %s within 2 minutes of automated greeting\n", info.SourceString())
			return true
		}
	}

	// Check if we've seen recent automated-looking messages from this sender
	cli.recentMessagesLock.RLock()
	defer cli.recentMessagesLock.RUnlock()

	// Look for patterns that suggest this might be after an automated greeting
	// This is a heuristic - you might want to adjust based on your specific use case
	now := time.Now()
	for _, key := range cli.recentMessagesList {
		if key.To == info.Sender && now.Sub(info.Timestamp) < 5*time.Second {
			// Recent message from same sender within 30 seconds - could be automated greeting scenario
			return true
		}
	}

	return false
}

// trackAutomatedGreeting should be called when we detect an automated greeting
func (cli *Client) trackAutomatedGreeting(sender types.JID) {
	cli.automatedGreetingTrackerLock.Lock()
	defer cli.automatedGreetingTrackerLock.Unlock()

	cli.automatedGreetingTracker[sender] = time.Now()
	fmt.Printf("DEBUG GREETINGS: Tracked automated greeting from %s\n", sender)

	// Clean up old entries (older than 10 minutes) to prevent memory leaks
	cutoff := time.Now().Add(-10 * time.Minute)
	for jid, timestamp := range cli.automatedGreetingTracker {
		if timestamp.Before(cutoff) {
			delete(cli.automatedGreetingTracker, jid)
		}
	}
}
