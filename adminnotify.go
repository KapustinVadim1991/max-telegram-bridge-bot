package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	maxschemes "github.com/max-messenger/max-bot-api-client-go/schemes"
)

// errNotifyCacheTTL — как часто перечитываем список получателей из БД.
const errNotifyCacheTTL = 60 * time.Second

// errorNotifyRecipients возвращает закэшированный список TG user id с флагом
// error_notify. Чтение из БД не чаще раза в errNotifyCacheTTL.
func (b *Bridge) errorNotifyRecipients() []int64 {
	b.errNotifyMu.Lock()
	defer b.errNotifyMu.Unlock()
	if b.errNotifyAt.IsZero() || time.Since(b.errNotifyAt) >= errNotifyCacheTTL {
		ids, err := b.repo.ErrorNotifyUsers()
		if err != nil {
			slog.Warn("error-notify: failed to load recipients", "err", err)
			return b.errNotifyIDs // отдаём предыдущий (возможно устаревший) кэш
		}
		b.errNotifyIDs = ids
		b.errNotifyAt = time.Now()
	}
	return b.errNotifyIDs
}

// notifyErrorAdmins асинхронно шлёт отчёт об ошибке доставки всем получателям с
// флагом error_notify. Текст собирается лениво в той же горутине (build), чтобы
// возможные доп. запросы (например, MAX GetChat для названия чата) не блокировали
// основную работу сервиса. Если получателей нет — build не вызывается.
func (b *Bridge) notifyErrorAdmins(build func(ctx context.Context) string) {
	go func() {
		ids := b.errorNotifyRecipients()
		if len(ids) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		text := build(ctx)
		if strings.TrimSpace(text) == "" {
			return
		}
		for _, id := range ids {
			if _, err := b.tg.SendMessage(ctx, id, text, &SendOpts{}); err != nil {
				slog.Warn("error-notify: send failed", "to", id, "err", err)
			}
		}
	}()
}

// excerpt обрезает строку до n символов (рун), добавляя многоточие.
func excerpt(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// tgMessageLink строит ссылку на сообщение в TG. Работает только для
// супергрупп/каналов (chat_id вида -100XXXXXXXXXX); для лички/обычных групп —
// пустая строка (у них нет ссылок на конкретное сообщение).
func tgMessageLink(chatID int64, msgID int) string {
	s := strconv.FormatInt(chatID, 10)
	if !strings.HasPrefix(s, "-100") {
		return ""
	}
	return fmt.Sprintf("https://t.me/c/%s/%d", strings.TrimPrefix(s, "-100"), msgID)
}

// maxToTgErrorReport собирает текст уведомления о неудачной пересылке MAX→TG.
// Название/тип/ссылку канала берём через MAX GetChat (сетевой вызов — поэтому
// функция вызывается внутри горутины notifyErrorAdmins). У MAX нет ссылок на
// конкретное сообщение, поэтому даём ссылку на сам чат, если он публичный.
func (b *Bridge) maxToTgErrorReport(ctx context.Context, msgUpd *maxschemes.MessageCreatedUpdate, reason string) string {
	chatID := msgUpd.Message.Recipient.ChatId
	title := ""
	ctype := string(msgUpd.Message.Recipient.ChatType)
	link := ""
	if chat, err := b.maxApi.Chats.GetChat(ctx, chatID); err == nil && chat != nil {
		if chat.Title != "" {
			title = chat.Title
		}
		if chat.Type != "" {
			ctype = string(chat.Type)
		}
		link = chat.Link
	}
	if title == "" {
		title = fmt.Sprintf("id %d", chatID)
	}

	var sb strings.Builder
	sb.WriteString("❌ Не доставлено MAX→TG\n")
	fmt.Fprintf(&sb, "Канал: «%s» (%s)\n", title, ctype)
	fmt.Fprintf(&sb, "Отправитель: %s (uid %d)\n", maxName(msgUpd), msgUpd.Message.Sender.UserId)
	fmt.Fprintf(&sb, "Ошибка: %s\n", reason)
	if text := strings.TrimSpace(msgUpd.Message.Body.Text); text != "" {
		fmt.Fprintf(&sb, "Текст: %s\n", excerpt(text, 200))
	}
	if link != "" {
		sb.WriteString(link)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// tgToMaxErrorReport собирает текст уведомления о неудачной пересылке TG→MAX.
// Для TG доступна ссылка на конкретное сообщение (для супергрупп/каналов).
func (b *Bridge) tgToMaxErrorReport(srcChat *TGMessage, reason string) string {
	if srcChat == nil {
		return ""
	}
	title := srcChat.Chat.Title
	if title == "" {
		title = fmt.Sprintf("id %d", srcChat.Chat.ID)
	}
	var uid int64
	if srcChat.From != nil {
		uid = srcChat.From.ID
	}
	text := srcChat.Text
	if text == "" {
		text = srcChat.Caption
	}

	var sb strings.Builder
	sb.WriteString("❌ Не доставлено TG→MAX\n")
	fmt.Fprintf(&sb, "Чат: «%s» (%s)\n", title, srcChat.Chat.Type)
	fmt.Fprintf(&sb, "Отправитель: %s (uid %d)\n", tgName(srcChat), uid)
	fmt.Fprintf(&sb, "Ошибка: %s\n", reason)
	if text != "" {
		fmt.Fprintf(&sb, "Текст: %s\n", excerpt(text, 200))
	}
	if link := tgMessageLink(srcChat.Chat.ID, srcChat.MessageID); link != "" {
		sb.WriteString(link)
	}
	return strings.TrimRight(sb.String(), "\n")
}
