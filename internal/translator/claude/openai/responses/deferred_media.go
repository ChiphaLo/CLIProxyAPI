package responses

import (
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
)

// Large media strings stay borrowed from the immutable input until the final
// document. Intermediate message arrays contain small request-private handles.
// All edits and escaping remain owned by ojson.Doc.
type deferredMedia struct {
	store *common.DeferredMedia
	// owned reports that this translation hydrates its own output; otherwise
	// the caller that supplied store hydrates the final document.
	owned bool
}

func newDeferredMedia(store *common.DeferredMedia) *deferredMedia {
	if store == nil {
		return &deferredMedia{store: &common.DeferredMedia{}, owned: true}
	}
	return &deferredMedia{store: store}
}

func (m *deferredMedia) set(doc *ojson.Doc, data string) {
	if m == nil {
		_ = doc.Set("source.data", data)
		return
	}
	_ = doc.Set("source.data", m.store.Handle(data))
}

func (m *deferredMedia) hydrate(doc *ojson.Doc) {
	if m == nil || !m.owned {
		return
	}
	m.store.Hydrate(doc)
}
