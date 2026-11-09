package channel

import (
	"context"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

func TestChannelLanguageForCommands(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}, Language: "fr"})
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "/help"))
	waitFor(t, "French help", hasText(r.a, "/new: commencer une nouvelle conversation"))
	waitFor(t, "French command help", hasText(r.a, "/retry: relancer mon dernier message"))
	r.g.Handle(ctx, dm("m2", "/new"))
	waitFor(t, "French reply", hasText(r.a, "Nouvelle conversation commencée."))
}

func TestGlobalLanguageAndChannelOverride(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}})
	r.g.Language = "es"
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "/stop"))
	waitFor(t, "Spanish reply", hasText(r.a, "No hay nada en marcha."))
	r.g.Channels["tg"].Language = "de"
	r.g.Handle(ctx, dm("m2", "/stop"))
	waitFor(t, "German override", hasText(r.a, "Es läuft nichts."))
}

func TestApprovalInTheChatsLanguage(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}, Language: "fr"})
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "please run it"))
	waitFor(t, "French approval prompt", hasText(r.a, "Autorisation requise"))
	r.g.Handle(ctx, dm("m2", "peut-être"))
	waitFor(t, "French reminder", hasText(r.a, "répondez **oui** ou **non**"))
	r.g.Handle(ctx, dm("m3", "Oui !"))
	waitFor(t, "approved in French", hasText(r.a, "Autorisé."))
	waitFor(t, "tool output", hasText(r.a, "approved-run"))
}

func TestEnglishApprovalWordsWorkEverywhere(t *testing.T) {
	r := newRig(t, &config.Channel{AllowFrom: []string{"7"}, Language: "ja"})
	ctx := context.Background()
	r.g.Handle(ctx, dm("m1", "run it"))
	waitFor(t, "Japanese approval prompt", hasText(r.a, "承認が必要です"))
	r.g.Handle(ctx, dm("m2", "yes"))
	waitFor(t, "tool output", hasText(r.a, "approved-run"))
}
