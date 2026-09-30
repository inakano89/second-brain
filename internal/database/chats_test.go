package database

import (
	"context"
	"errors"
	"testing"
)

func TestChats(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	if list, err := db.ListChats(ctx); err != nil || len(list) != 0 {
		t.Fatalf("fresh db has chats: %+v %v", list, err)
	}
	a, err := db.CreateChat(ctx, "  Joelho ", "medico", "")
	if err != nil || a.Title != "Joelho" || a.Persona != "medico" {
		t.Fatalf("create: %+v %v", a, err)
	}
	b, _ := db.CreateChat(ctx, "", personaCustom, " fale como coach ")
	if b.ID <= a.ID || b.Instructions != "fale como coach" {
		t.Fatalf("second chat: %+v", b)
	}
	if list, _ := db.ListChats(ctx); len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID {
		t.Fatalf("list must keep creation order: %+v", list)
	}

	// The first message names a chat that has no title; a title already set is kept.
	if ok, _ := db.AutoTitleChat(ctx, b.ID, "Corrida de domingo"); !ok {
		t.Fatal("auto title should apply to an untitled chat")
	}
	if ok, _ := db.AutoTitleChat(ctx, b.ID, "Outra coisa"); ok {
		t.Fatal("auto title must not overwrite")
	}
	if ok, _ := db.AutoTitleChat(ctx, a.ID, "x"); ok {
		t.Fatal("auto title must not overwrite a title chosen by the user")
	}
	if err := db.RenameChat(ctx, b.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetChat(ctx, b.ID); got.Title != "" {
		t.Fatalf("empty rename goes back to automatic: %q", got.Title)
	}
	if err := db.RenameChat(ctx, 999, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename missing: %v", err)
	}

	// Histories are separate per chat and go away with it.
	db.AppendChat(ctx, ChatChannel(a.ID), "user", "oi")
	db.AppendChat(ctx, ChatChannel(a.ID), "assistant", "olá")
	db.AppendChat(ctx, ChatChannel(b.ID), "user", "outro")
	if h, _ := db.ChatHistory(ctx, ChatChannel(a.ID), 10); len(h) != 2 {
		t.Fatalf("history a: %+v", h)
	}
	if err := db.DeleteChat(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if h, _ := db.ChatHistory(ctx, ChatChannel(a.ID), 10); len(h) != 0 {
		t.Fatalf("deleted chat kept messages: %+v", h)
	}
	if h, _ := db.ChatHistory(ctx, ChatChannel(b.ID), 10); len(h) != 1 {
		t.Fatalf("delete touched another chat: %+v", h)
	}
	if _, err := db.GetChat(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted: %v", err)
	}
	if err := db.DeleteChat(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

// personaCustom mirrors agent.PersonaCustom (database cannot import agent).
const personaCustom = "custom"

// TestChatsMigrationKeepsHistory: the single web history that existed before tabs becomes chat 1.
func TestChatsMigrationKeepsHistory(t *testing.T) {
	ctx := context.Background()
	run := func(db *DB) {
		t.Helper()
		for _, st := range splitSQL(migrations[8])[1:] { // migration 9 without CREATE TABLE
			if _, err := db.ExecContext(ctx, st); err != nil {
				t.Fatal(err)
			}
		}
	}

	db := openTest(t)
	db.AppendChat(ctx, "web", "user", "pergunta antiga")
	db.AppendChat(ctx, "web", "assistant", "resposta antiga")
	db.AppendChat(ctx, "tg:1", "user", "telegram fica como está")
	run(db)
	c, err := db.GetChat(ctx, 1)
	if err != nil || c.Persona != "" {
		t.Fatalf("legacy chat: %+v %v", c, err)
	}
	if h, _ := db.ChatHistory(ctx, ChatChannel(1), 10); len(h) != 2 || h[0].Content != "pergunta antiga" {
		t.Fatalf("history not moved: %+v", h)
	}
	if h, _ := db.ChatHistory(ctx, "tg:1", 10); len(h) != 1 {
		t.Fatalf("telegram history touched: %+v", h)
	}

	empty := openTest(t) // no web history: no phantom chat
	run(empty)
	if list, _ := empty.ListChats(ctx); len(list) != 0 {
		t.Fatalf("chat created without history: %+v", list)
	}
}
