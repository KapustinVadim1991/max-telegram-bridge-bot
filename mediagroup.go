package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	maxbot "github.com/max-messenger/max-bot-api-client-go"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const mediaGroupTimeout = 1 * time.Second

// mediaGroupItem хранит данные одного сообщения из альбома TG.
type mediaGroupItem struct {
	photoSizes []tgbotapi.PhotoSize
	fileID     string // для видео/документов (альбомы могут содержать видео)
	caption    string
	replyToMsg *tgbotapi.Message
	entities   []tgbotapi.MessageEntity
	msg        *tgbotapi.Message
}

// mediaGroupBuffer накапливает сообщения альбома перед отправкой.
type mediaGroupBuffer struct {
	mu       sync.Mutex
	items    []mediaGroupItem
	chatID   int64 // TG chat ID (для определения maxChatID)
	timer    *time.Timer
	fired    bool
}

// bufferMediaGroup добавляет сообщение в буфер альбома.
// Если это первое сообщение — запускает таймер.
func (b *Bridge) bufferMediaGroup(ctx context.Context, groupID string, item mediaGroupItem, maxChatID int64) {
	b.mgMu.Lock()

	buf, ok := b.mgBuffers[groupID]
	if !ok {
		buf = &mediaGroupBuffer{
			chatID: item.msg.Chat.ID,
		}
		b.mgBuffers[groupID] = buf
		buf.timer = time.AfterFunc(mediaGroupTimeout, func() {
			b.flushMediaGroup(ctx, groupID)
		})
	}

	b.mgMu.Unlock()

	buf.mu.Lock()
	buf.items = append(buf.items, item)
	buf.mu.Unlock()
}

// flushMediaGroup отправляет все накопленные фото альбома одним сообщением в MAX.
func (b *Bridge) flushMediaGroup(ctx context.Context, groupID string) {
	b.mgMu.Lock()
	buf, ok := b.mgBuffers[groupID]
	if !ok {
		b.mgMu.Unlock()
		return
	}
	delete(b.mgBuffers, groupID)
	b.mgMu.Unlock()

	buf.mu.Lock()
	buf.timer.Stop()
	items := buf.items
	buf.mu.Unlock()

	if len(items) == 0 {
		return
	}

	// Определяем maxChatID
	maxChatID, linked := b.repo.GetMaxChat(items[0].msg.Chat.ID)
	if !linked {
		slog.Warn("media group: chat not linked", "tgChat", items[0].msg.Chat.ID)
		return
	}

	uid := tgUserID(items[0].msg)
	prefix := b.repo.HasPrefix("tg", items[0].msg.Chat.ID)

	// Caption и entities берём из первого элемента, у которого caption не пустой
	var caption string
	var entities []tgbotapi.MessageEntity
	for _, it := range items {
		if it.caption != "" {
			caption = it.caption
			entities = it.entities
			break
		}
	}

	// Reply ID из первого элемента с reply
	var replyTo string
	for _, it := range items {
		if it.replyToMsg != nil {
			if maxReplyID, ok := b.repo.LookupMaxMsgID(it.msg.Chat.ID, it.replyToMsg.MessageID); ok {
				replyTo = maxReplyID
			}
			break
		}
	}

	// Форматируем caption
	mdCaption := caption
	if entities != nil {
		mdCaption = tgEntitiesToMarkdown(caption, entities)
	}

	m := maxbot.NewMessage().SetChat(maxChatID).SetText(mdCaption)
	if replyTo != "" {
		m.SetReply(mdCaption, replyTo)
	}

	// Загружаем и добавляем все фото
	photosSent := 0
	for _, it := range items {
		if len(it.photoSizes) > 0 {
			photo := it.photoSizes[len(it.photoSizes)-1]
			fileURL, err := b.tgFileURL(photo.FileID)
			if err != nil {
				slog.Error("media group: tgFileURL failed", "err", err)
				continue
			}
			uploaded, err := b.maxApi.Uploads.UploadPhotoFromUrl(ctx, fileURL)
			if err != nil {
				slog.Error("media group: photo upload failed", "err", err)
				continue
			}
			m.AddPhoto(uploaded)
			photosSent++
		}
	}

	if photosSent == 0 {
		slog.Warn("media group: no photos uploaded, skipping")
		return
	}

	slog.Info("TG→MAX sending media group", "photos", photosSent, "uid", uid, "tgChat", items[0].msg.Chat.ID, "maxChat", maxChatID)
	result, err := b.maxApi.Messages.SendWithResult(ctx, m)
	if err != nil {
		slog.Error("TG→MAX media group send failed", "err", err, "uid", uid, "tgChat", items[0].msg.Chat.ID, "maxChat", maxChatID)
		if b.cbFail(maxChatID) {
			b.tgBot.Send(tgbotapi.NewMessage(items[0].msg.Chat.ID,
				"Не удалось переслать альбом в MAX."))
		}
		// Отправляем по одному как fallback
		for _, it := range items {
			go b.forwardTgToMax(ctx, it.msg, maxChatID, formatTgCaption(it.msg, prefix))
		}
		return
	}

	b.cbSuccess(maxChatID)
	slog.Info("TG→MAX media group sent", "mid", result.Body.Mid, "photos", photosSent)
	// Сохраняем маппинг для первого сообщения группы (для reply)
	b.repo.SaveMsg(items[0].msg.Chat.ID, items[0].msg.MessageID, maxChatID, result.Body.Mid)
}
