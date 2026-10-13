//go:build !linux

package quota

import (
	"context"
	"errors"
)

type Store struct{}

func OpenStore(context.Context, string, uint32, uint32) (*Store, error) {
	return nil, errors.New("quota persistence requires Linux")
}
func (*Store) Previous() (Snapshot, error) {
	return Snapshot{}, errors.New("quota persistence requires Linux")
}
func (*Store) Save(Snapshot) error          { return errors.New("quota persistence requires Linux") }
func (*Store) AppendHistory(Snapshot) error { return errors.New("quota persistence requires Linux") }
func (*Store) Close() error                 { return nil }
