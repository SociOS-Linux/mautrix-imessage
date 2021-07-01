// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2021 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package ios

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/gabriel-vasile/mimetype"
	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/mautrix-imessage/ipc"
)

const (
	IncomingMessage            ipc.Command = "message"
	IncomingReadReceipt        ipc.Command = "read_receipt"
	IncomingTypingNotification ipc.Command = "typing"
)

func floatToTime(unix float64) time.Time {
	sec, dec := math.Modf(unix)
	return time.Unix(int64(sec), int64(dec*(1e9)))
}

func timeToFloat(time time.Time) float64 {
	return float64(time.Unix()) + float64(time.Nanosecond())/1e9
}

type APIWithIPC interface {
	imessage.API
	SetIPC(*ipc.Processor)
}

type iOSConnector struct {
	IPC         *ipc.Processor
	log         log.Logger
	messageChan chan *imessage.Message
	receiptChan chan *imessage.ReadReceipt
	typingChan  chan *imessage.TypingNotification
}

func NewPlainiOSConnector(logger log.Logger) APIWithIPC {
	return &iOSConnector{
		log:         logger,
		messageChan: make(chan *imessage.Message, 256),
		receiptChan: make(chan *imessage.ReadReceipt, 32),
		typingChan:  make(chan *imessage.TypingNotification, 32),
	}
}

func NewiOSConnector(bridge imessage.Bridge) (imessage.API, error) {
	ios := NewPlainiOSConnector(bridge.GetLog().Sub("iMessage").Sub("iOS"))
	ios.SetIPC(bridge.GetIPC())
	return ios, nil
}

func init() {
	imessage.Implementations["ios"] = NewiOSConnector
	imessage.Implementations["android"] = NewiOSConnector
}

func (ios *iOSConnector) SetIPC(proc *ipc.Processor) {
	ios.IPC = proc
}

func (ios *iOSConnector) Start() error {
	ios.IPC.SetHandler(IncomingMessage, ios.handleIncomingMessage)
	ios.IPC.SetHandler(IncomingReadReceipt, ios.handleIncomingReadReceipt)
	ios.IPC.SetHandler(IncomingTypingNotification, ios.handleIncomingTypingNotification)
	return nil
}

func (ios *iOSConnector) Stop() {}

func (ios *iOSConnector) postprocessMessage(message *imessage.Message) {
	if !message.IsFromMe {
		message.Sender = imessage.ParseIdentifier(message.JSONSenderGUID)
	}
	message.Time = floatToTime(message.JSONUnixTime)
	if message.Tapback != nil {
		_, err := message.Tapback.Parse()
		if err != nil {
			ios.log.Warnfln("Failed to parse tapback in %s: %v", message.GUID, err)
		}
	}
	if len(message.NewGroupName) > 0 {
		message.GroupActionType = imessage.GroupActionSetName
	}
	if message.Attachment != nil && message.Attachments == nil {
		message.Attachments = []*imessage.Attachment{message.Attachment}
	} else if message.Attachments != nil && len(message.Attachments) > 0 && message.Attachment == nil {
		message.Attachment = message.Attachments[0]
	}
}

func (ios *iOSConnector) handleIncomingMessage(data json.RawMessage) interface{} {
	var message imessage.Message
	err := json.Unmarshal(data, &message)
	if err != nil {
		ios.log.Warnln("Failed to parse incoming message: %v", err)
		return nil
	}
	ios.postprocessMessage(&message)
	select {
	case ios.messageChan <- &message:
	default:
		ios.log.Warnln("Incoming message buffer is full")
	}
	return nil
}

func (ios *iOSConnector) handleIncomingReadReceipt(data json.RawMessage) interface{} {
	var receipt imessage.ReadReceipt
	err := json.Unmarshal(data, &receipt)
	if err != nil {
		ios.log.Warnln("Failed to parse incoming read receipt: %v", err)
		return nil
	}

	select {
	case ios.receiptChan <- &receipt:
	default:
		ios.log.Warnln("Incoming receipt buffer is full")
	}
	return nil
}

func (ios *iOSConnector) handleIncomingTypingNotification(data json.RawMessage) interface{} {
	var notif imessage.TypingNotification
	err := json.Unmarshal(data, &notif)
	if err != nil {
		ios.log.Warnln("Failed to parse incoming typing notification: %v", err)
		return nil
	}
	select {
	case ios.typingChan <- &notif:
	default:
		ios.log.Warnln("Incoming typing notification buffer is full")
	}
	return nil
}

func (ios *iOSConnector) GetMessagesSinceDate(chatID string, minDate time.Time) ([]*imessage.Message, error) {
	resp := make([]*imessage.Message, 0)
	err := ios.IPC.Request(context.Background(), ReqGetRecentMessages, &GetMessagesAfterRequest{
		ChatGUID:  chatID,
		Timestamp: timeToFloat(minDate),
	}, &resp)
	for _, msg := range resp {
		ios.postprocessMessage(msg)
	}
	return resp, err
}

func (ios *iOSConnector) GetMessagesWithLimit(chatID string, limit int) ([]*imessage.Message, error) {
	resp := make([]*imessage.Message, 0)
	err := ios.IPC.Request(context.Background(), ReqGetRecentMessages, &GetRecentMessagesRequest{
		ChatGUID: chatID,
		Limit:    limit,
	}, &resp)
	for _, msg := range resp {
		ios.postprocessMessage(msg)
	}
	return resp, err
}

func (ios *iOSConnector) GetChatsWithMessagesAfter(minDate time.Time) (resp []string, err error) {
	return resp, ios.IPC.Request(context.Background(), ReqGetChats, &GetChatsRequest{
		MinTimestamp: timeToFloat(minDate),
	}, &resp)
}

func (ios *iOSConnector) MessageChan() <-chan *imessage.Message {
	return ios.messageChan
}

func (ios *iOSConnector) ReadReceiptChan() <-chan *imessage.ReadReceipt {
	return ios.receiptChan
}

func (ios *iOSConnector) TypingNotificationChan() <-chan *imessage.TypingNotification {
	return ios.typingChan
}

func (ios *iOSConnector) GetContactInfo(identifier string) (*imessage.Contact, error) {
	var resp imessage.Contact
	err := ios.IPC.Request(context.Background(), ReqGetContact, &GetContactRequest{UserGUID: identifier}, &resp)
	if len(resp.AvatarB64) > 0 {
		var b64err error
		resp.Avatar, b64err = base64.StdEncoding.DecodeString(resp.AvatarB64)
		if b64err != nil {
			ios.log.Warnfln("Failed to decode avatar of %s: %v", identifier, b64err)
		}
	}
	return &resp, err
}

func (ios *iOSConnector) GetChatInfo(chatID string) (*imessage.ChatInfo, error) {
	var resp imessage.ChatInfo
	err := ios.IPC.Request(context.Background(), ReqGetChat, &GetChatRequest{ChatGUID: chatID}, &resp)
	return &resp, err
}

func (ios *iOSConnector) GetGroupAvatar(chatID string) (*imessage.Attachment, error) {
	var resp imessage.Attachment
	err := ios.IPC.Request(context.Background(), ReqGetChatAvatar, &GetChatRequest{ChatGUID: chatID}, &resp)
	return &resp, err
}

func (ios *iOSConnector) SendMessage(chatID, text string) (*imessage.SendResponse, error) {
	var resp imessage.SendResponse
	err := ios.IPC.Request(context.Background(), ReqSendMessage, &SendMessageRequest{
		ChatGUID: chatID,
		Text:     text,
	}, &resp)
	return &resp, err
}

func (ios *iOSConnector) SendFile(chatID, filename string, data []byte) (*imessage.SendResponse, error) {
	dir, err := ioutil.TempDir("", "mautrix-imessage-upload")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.Remove(dir)
	filePath := filepath.Join(dir, filename)
	err = ioutil.WriteFile(filePath, data, 0640)
	if err != nil {
		return nil, fmt.Errorf("failed to write data to temp file: %w", err)
	}
	defer os.Remove(filePath)

	var resp imessage.SendResponse
	err = ios.IPC.Request(context.Background(), ReqSendMedia, &SendMediaRequest{
		ChatGUID: chatID,
		Attachment: imessage.Attachment{
			FileName:   filename,
			PathOnDisk: filePath,
			MimeType:   mimetype.Detect(data).String(),
		},
	}, &resp)
	return &resp, err
}

func (ios *iOSConnector) SendTapback(chatID, targetGUID string, tapback imessage.TapbackType, remove bool) (*imessage.SendResponse, error) {
	if remove {
		tapback += imessage.TapbackRemoveOffset
	}
	var resp imessage.SendResponse
	err := ios.IPC.Request(context.Background(), ReqSendTapback, &SendTapbackRequest{
		ChatGUID:   chatID,
		TargetGUID: targetGUID,
		Type:       tapback,
	}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, err
}

func (ios *iOSConnector) SendReadReceipt(chatID, readUpTo string) error {
	return ios.IPC.Send(ReqSendReadReceipt, &SendReadReceiptRequest{
		ChatGUID: chatID,
		ReadUpTo: readUpTo,
	})
}

func (ios *iOSConnector) SendTypingNotification(chatID string, typing bool) error {
	return ios.IPC.Send(ReqSetTyping, &SetTypingRequest{
		ChatGUID: chatID,
		Typing:   typing,
	})
}

func (ios *iOSConnector) Capabilities() imessage.ConnectorCapabilities {
	return imessage.ConnectorCapabilities{
		MessageSendResponses:    true,
		SendTapbacks:            true,
		SendReadReceipts:        true,
		SendTypingNotifications: true,
	}
}
