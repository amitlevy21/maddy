package maddy

import (
	"context"
	"sync"

	"github.com/emersion/go-message/textproto"
	"github.com/foxcpp/maddy/buffer"
	"github.com/foxcpp/maddy/config"
	"github.com/foxcpp/maddy/module"
)

type testCheck struct {
	connErr   error
	senderErr error
	rcptErr   error
	bodyErr   error
}

func (tc *testCheck) NewMessage(msgMeta *module.MsgMetadata) (module.CheckState, error) {
	return &testCheckState{msgMeta, tc}, nil
}

func (tc *testCheck) Init(*config.Map) error {
	return nil
}

func (tc *testCheck) Name() string {
	return "test_check"
}

func (tc *testCheck) InstanceName() string {
	return "test_check"
}

type testCheckState struct {
	msgMeta *module.MsgMetadata
	check   *testCheck
}

func (tcs *testCheckState) CheckConnection(ctx context.Context) error {
	return tcs.check.connErr
}

func (tcs *testCheckState) CheckSender(ctx context.Context, from string) error {
	return tcs.check.senderErr
}

func (tcs *testCheckState) CheckRcpt(ctx context.Context, to string) error {
	return tcs.check.rcptErr
}

func (tcs *testCheckState) CheckBody(ctx context.Context, headerLock *sync.RWMutex, header textproto.Header, body buffer.Buffer) error {
	return tcs.check.bodyErr
}

func (tcs *testCheckState) Close() error {
	return nil
}

func init() {
	module.Register("test_check", func(modName, instanceName string, aliases []string) (module.Module, error) {
		return &testCheck{}, nil
	})
	module.RegisterInstance(&testCheck{}, nil)
}
