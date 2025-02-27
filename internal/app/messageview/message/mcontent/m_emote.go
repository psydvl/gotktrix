package mcontent

import (
	"context"
	_ "embed"

	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/diamondburned/gotkit/app/locale"
	"github.com/diamondburned/gotkit/gtkutil/cssutil"
	"github.com/diamondburned/gotktrix/internal/app/messageview/message/mauthor"
	"github.com/diamondburned/gotktrix/internal/gotktrix"
	"github.com/diamondburned/gotrix/event"
)

type emoteContent struct {
	*gtk.Label
}

//go:embed styles/mcontent-emote.css
var emoteContentStyle string
var emoteContentCSS = cssutil.Applier("mcontent-emote", emoteContentStyle)

func newEmoteContent(ctx context.Context, msg *event.RoomMessageEvent) emoteContent {
	author := mauthor.Name(gotktrix.FromContext(ctx), msg.RoomID, msg.Sender)

	l := gtk.NewLabel(author + locale.S(ctx, " ") + msg.Body)
	l.SetXAlign(0)
	l.SetWrap(true)
	l.SetWrapMode(pango.WrapWordChar)
	emoteContentCSS(l)

	return emoteContent{l}
}

func (c emoteContent) content() {}
