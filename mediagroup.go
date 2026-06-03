package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
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

// flushMediaGroup отправляет каждый файл альбома отдельным сообщением в MAX.
//
// Почему по одному, а не одним batch-запросом:
//  1. MAX API поддерживает только одно вложение на сообщение — несколько AddPhoto()
//     в одном запросе либо игнорируются, либо вызывают ошибку.
//  2. Документы (PDF и др.) не попадали в старый batch-путь вообще, потому что
//     mediaGroupItem хранит только photoSizes/videoFileID, а Document — нет.
//     item.msg (оригинальный *TGMessage) несёт все поля включая Document,
//     и forwardTgToMax уже умеет их обрабатывать.
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

	prefix := !isCrosspost && b.hasPrefix("tg", items[0].msg.Chat.ID)

	slog.Info("TG→MAX flushing media group", "items", len(items),
		"tgChat", items[0].msg.Chat.ID, "maxChat", maxChatID)

	// Отправляем каждый файл отдельным сообщением.
	// forwardTgToMax использует item.msg напрямую и корректно обрабатывает
	// Photo, Video, Document и все остальные типы вложений.
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
}
