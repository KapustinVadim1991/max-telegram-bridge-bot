package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	maxbot "github.com/max-messenger/max-bot-api-client-go"
	maxschemes "github.com/max-messenger/max-bot-api-client-go/schemes"
)

const mediaGroupTimeout = 1 * time.Second

// mediaGroupItem хранит данные одного сообщения из альбома TG.
type mediaGroupItem struct {
	photoSizes  []PhotoSize
	videoFileID string // для видео в альбомах
	caption     string
	replyToMsg  *TGMessage
	entities    []Entity
	msg         *TGMessage
	maxChatID   int64 // если задан — используется напрямую (crosspost)
	crosspost   bool  // кросспостинг: без prefix, другой caption формат
}

// mediaGroupBuffer накапливает сообщения альбома перед отправкой.
type mediaGroupBuffer struct {
	mu    sync.Mutex
	items []mediaGroupItem
	timer *time.Timer
}

// bufferMediaGroup добавляет сообщение в буфер альбома.
// Если это первое сообщение — запускает таймер.
func (b *Bridge) bufferMediaGroup(ctx context.Context, groupID string, item mediaGroupItem) {
	b.mgMu.Lock()

	buf, ok := b.mgBuffers[groupID]
	if !ok {
		buf = &mediaGroupBuffer{}
		b.mgBuffers[groupID] = buf
		// Добавляем первый item до запуска таймера — исключает гонку
		buf.items = append(buf.items, item)
		buf.timer = time.AfterFunc(mediaGroupTimeout, func() {
			b.flushMediaGroup(ctx, groupID)
		})
		b.mgMu.Unlock()
		return
	}

	b.mgMu.Unlock()

	buf.mu.Lock()
	buf.items = append(buf.items, item)
	buf.mu.Unlock()
}

// flushMediaGroup отправляет накопленные файлы альбома в MAX.
// Фото — одним batch-сообщением (MAX поддерживает несколько photo attachments).
// Видео и документы — отдельными сообщениями, т.к. MAX не поддерживает batch для них.
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
	isCrosspost := items[0].crosspost
	maxChatID := items[0].maxChatID
	if maxChatID == 0 {
		var linked bool
		maxChatID, linked = b.repo.GetMaxChat(items[0].msg.Chat.ID)
		if !linked {
			slog.Warn("media group: chat not linked", "tgChat", items[0].msg.Chat.ID)
			return
		}
	}

	uid := tgUserID(items[0].msg)
	prefix := !isCrosspost && b.hasPrefix("tg", items[0].msg.Chat.ID)

	// --- Caption ---
	// Ищем элемент с реальным текстом (не просто attribution).
	// Баг оригинала: it.caption != "" всегда истинно, т.к. formatTgCaption
	// возвращает минимум "Name: " — поэтому всегда брался первый элемент.
	var mdCaption string
	captionFound := false
	for _, it := range items {
		rawText := it.msg.Caption
		ents := it.msg.CaptionEntities
		if rawText == "" {
			rawText = it.msg.Text
			ents = it.msg.Entities
		}
		if rawText == "" {
			continue
		}
		// Нашли элемент с текстом — строим mdCaption правильно:
		// сначала entities→markdown на сыром тексте, потом attribution.
		if isCrosspost {
			// crosspost: caption уже готов в it.caption (formatTgCrosspostCaption)
			mdCaption = it.caption
		} else {
			mdText := tgEntitiesToMarkdown(rawText, ents)
			name := tgName(it.msg)
			if prefix {
				name = "[TG] " + name
			}
			mdCaption = formatAttributionMD(name, mdText, b.cfg.MessageNewline)
		}
		captionFound = true
		break
	}
	// Если ни один элемент не имеет текста — только attribution
	if !captionFound && len(items) > 0 && !isCrosspost {
		name := tgName(items[0].msg)
		if prefix {
			name = "[TG] " + name
		}
		mdCaption = formatAttributionMD(name, "", b.cfg.MessageNewline)
	}

	// --- Reply ID ---
	var replyTo string
	for _, it := range items {
		if it.replyToMsg != nil {
			if maxReplyID, ok := b.repo.LookupMaxMsgID(it.msg.Chat.ID, it.replyToMsg.MessageID); ok {
				replyTo = maxReplyID
			}
			break
		}
	}

	m := maxbot.NewMessage().SetChat(maxChatID).SetText(mdCaption)
	m.SetFormat("markdown")
	if replyTo != "" {
		m.SetReply(mdCaption, replyTo)
	}

	// --- Фото (batch) ---
	photosSent := 0
	var photoFailErr error
	for _, it := range items {
		if len(it.photoSizes) == 0 {
			continue
		}
		photo := it.photoSizes[len(it.photoSizes)-1]
		if b.cfg.TgAPIURL != "" {
			uploaded, err := b.uploadTgPhotoToMax(ctx, photo.FileID)
			if err != nil {
				slog.Error("media group: photo upload failed", "err", err)
				photoFailErr = err
				continue
			}
			m.AddPhoto(uploaded)
		} else {
			fileURL, err := b.tgFileURL(ctx, photo.FileID)
			if err != nil {
				slog.Error("media group: tgFileURL failed", "err", err)
				photoFailErr = err
				continue
			}
			uploaded, err := b.maxApi.Uploads.UploadPhotoFromUrl(ctx, fileURL)
			if err != nil {
				slog.Error("media group: photo upload failed", "err", err)
				photoFailErr = err
				continue
			}
			m.AddPhoto(uploaded)
		}
		photosSent++
	}
	if photoFailErr != nil && photosSent == 0 {
		b.notifyTgUser(ctx, items[0].msg, maxChatID,
			uploadErrMsg("Не удалось отправить альбом в MAX", photoFailErr), isCrosspost)
	}

	// --- Видео ---
	videosSent := 0
	var videoTokens []string
	for _, it := range items {
		if it.videoFileID == "" {
			continue
		}
		uploaded, err := b.uploadTgMediaToMax(ctx, it.videoFileID, maxschemes.VIDEO, "video.mp4")
		if err != nil {
			slog.Error("media group: video upload failed", "err", err)
			continue
		}
		videoTokens = append(videoTokens, uploaded.Token)
		videosSent++
	}

	// --- Документы (PDF, xlsx и др.) ---
	// В оригинале не обрабатывались — it.msg.Document есть, но photoSizes/videoFileID пустые.
	docsSent := 0
	for _, it := range items {
		if it.msg.Document == nil {
			continue
		}
		doc := it.msg.Document
		name := doc.FileName
		uploadType := maxschemes.FILE
		attType := "file"

		if strings.HasPrefix(doc.MimeType, "video/") {
			uploadType = maxschemes.VIDEO
			attType = "video"
			if name == "" {
				name = mimeToFilename("video", doc.MimeType)
			}
		}
		if name == "" {
			name = mimeToFilename("document", doc.MimeType)
		}

		uploaded, err := b.uploadTgMediaToMax(ctx, doc.FileID, uploadType, name)
		if err != nil {
			slog.Error("media group: doc upload failed", "err", err, "name", name)
			continue
		}

		// Caption только на первый документ
		docCap := ""
		if docsSent == 0 {
			docCap = mdCaption
		}

		mid, err := b.sendMaxDirectFormatted(ctx, maxChatID, docCap, attType, uploaded.Token, replyTo, "markdown")
		if err != nil {
			slog.Error("TG→MAX media group doc send failed", "err", err, "name", name)
			continue
		}
		if docsSent == 0 {
			b.repo.SaveMsg(it.msg.Chat.ID, it.msg.MessageID, maxChatID, mid, it.msg.MessageThreadID)
		}
		docsSent++
		slog.Info("TG→MAX media group doc sent", "name", name, "mid", mid)
	}

	totalMedia := photosSent + videosSent + docsSent
	if totalMedia == 0 {
		slog.Warn("media group: no media uploaded, skipping",
			"tgChat", items[0].msg.Chat.ID, "maxChat", maxChatID)
		return
	}

	slog.Info("TG→MAX sending media group",
		"photos", photosSent, "videos", videosSent, "docs", docsSent,
		"uid", uid, "tgChat", items[0].msg.Chat.ID, "maxChat", maxChatID)

	// --- Отправка фото (batch) ---
	if photosSent > 0 {
		result, err := b.maxApi.Messages.SendWithResult(ctx, m)
		if err != nil {
			slog.Error("TG→MAX media group send failed", "err", err)
			if b.cbFail(maxChatID) {
				b.notifyTgUser(ctx, items[0].msg, maxChatID,
					fmt.Sprintf("Не удалось переслать альбом в MAX. Пересылка приостановлена на %d мин. Проверьте, что бот добавлен в MAX-чат и является админом.", int(cbCooldown.Minutes())), isCrosspost)
			}
			// Fallback — по одному
			for _, it := range items {
				var cap string
				if isCrosspost {
					cap = formatTgCrosspostCaption(it.msg)
					repl := b.repo.GetCrosspostReplacements(maxChatID)
					if len(repl.TgToMax) > 0 {
						cap = applyReplacements(cap, repl.TgToMax)
					}
				} else {
					cap = formatTgCaption(it.msg, prefix, b.cfg.MessageNewline)
				}
				go b.forwardTgToMax(ctx, it.msg, maxChatID, cap, isCrosspost)
			}
			return
		}
		b.cbSuccess(maxChatID)
		slog.Info("TG→MAX media group sent", "mid", result.Body.Mid, "photos", photosSent)
		b.repo.SaveMsg(items[0].msg.Chat.ID, items[0].msg.MessageID, maxChatID, result.Body.Mid, items[0].msg.MessageThreadID)
	}

	// --- Отправка видео (по одному) ---
	for i, token := range videoTokens {
		videoCaption := ""
		if i == 0 && photosSent == 0 {
			videoCaption = mdCaption
		}
		mid, err := b.sendMaxDirectFormatted(ctx, maxChatID, videoCaption, "video", token, replyTo, "markdown")
		if err != nil {
			slog.Error("TG→MAX media group video send failed", "err", err)
			continue
		}
		if i == 0 && photosSent == 0 {
			b.repo.SaveMsg(items[0].msg.Chat.ID, items[0].msg.MessageID, maxChatID, mid, items[0].msg.MessageThreadID)
		}
	}
}
