package maddy

import (
	"context"
	"sync"

	"github.com/emersion/go-message/textproto"
	"github.com/foxcpp/maddy/buffer"
	"github.com/foxcpp/maddy/module"
	"golang.org/x/sync/errgroup"
)

// CheckGroup is a type that wraps a group of checks and runs them in parallel.
//
// It implements module.Check interface.
type CheckGroup struct {
	checks []module.Check
}

func (cg *CheckGroup) NewMessage(msgMeta *module.MsgMetadata) (module.CheckState, error) {
	states := make([]module.CheckState, 0, len(cg.checks))
	for _, check := range cg.checks {
		state, err := check.NewMessage(msgMeta)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return &checkGroupState{msgMeta, states}, nil
}

type checkGroupState struct {
	msgMeta *module.MsgMetadata
	states  []module.CheckState
}

func (cgs *checkGroupState) CheckConnection(ctx context.Context) error {
	syncGroup, childCtx := errgroup.WithContext(ctx)
	for _, state := range cgs.states {
		state := state
		syncGroup.Go(func() error { return state.CheckConnection(childCtx) })
	}
	return syncGroup.Wait()
}

func (cgs *checkGroupState) CheckSender(ctx context.Context, from string) error {
	syncGroup, childCtx := errgroup.WithContext(ctx)
	for _, state := range cgs.states {
		state := state
		syncGroup.Go(func() error { return state.CheckSender(childCtx, from) })
	}
	return syncGroup.Wait()
}

func (cgs *checkGroupState) CheckRcpt(ctx context.Context, to string) error {
	syncGroup, childCtx := errgroup.WithContext(ctx)
	for _, state := range cgs.states {
		state := state
		syncGroup.Go(func() error { return state.CheckRcpt(childCtx, to) })
	}
	return syncGroup.Wait()
}

func (cgs *checkGroupState) CheckBody(ctx context.Context, headerLock *sync.RWMutex, header textproto.Header, body buffer.Buffer) error {
	syncGroup, childCtx := errgroup.WithContext(ctx)
	for _, state := range cgs.states {
		state := state
		syncGroup.Go(func() error { return state.CheckBody(childCtx, headerLock, header, body) })
	}
	return syncGroup.Wait()
}

func (cgs *checkGroupState) Close() error {
	for _, state := range cgs.states {
		state.Close()
	}
	return nil
}
