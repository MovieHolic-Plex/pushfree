package fcm

import (
	"context"
	"os"
	"testing"
)

// TestIntegration_SendReal is the skeleton for a live FCM v1 round-trip. It is
// gated behind PUSHFREE_FCM_TEST=1 AND a real service-account path so it never
// runs in CI or on machines without real credentials. It is intentionally a
// skeleton: to exercise the live API, set PUSHFREE_FCM_CREDENTIALS to a real
// service-account JSON path and PUSHFREE_FCM_TEST_TOKEN to a disposable device
// token, then run with -count=1.
//
// No real credentials exist on this machine, so this test always skips here.
func TestIntegration_SendReal(t *testing.T) {
	if os.Getenv("PUSHFREE_FCM_TEST") != "1" {
		t.Skip("skipping live FCM integration (set PUSHFREE_FCM_TEST=1 and PUSHFREE_FCM_CREDENTIALS to run)")
	}
	credPath := os.Getenv("PUSHFREE_FCM_CREDENTIALS")
	if credPath == "" {
		t.Fatal("PUSHFREE_FCM_TEST=1 set but PUSHFREE_FCM_CREDENTIALS is empty")
	}

	ctx := context.Background()
	c, err := New(ctx, Options{CredentialsPath: credPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	token := os.Getenv("PUSHFREE_FCM_TEST_TOKEN")
	if token == "" {
		t.Skip("PUSHFREE_FCM_TEST_TOKEN not set; nothing to deliver to")
	}

	res, err := c.Send(ctx, Outbound{
		DeviceID: "integration-test",
		Token:    token,
		Priority: 0,
		Data:     map[string]string{"ping": "fcm-integration"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	t.Logf("send result: state=%s http=%d reason=%q", res.State, res.HTTP, res.Reason)
	if res.State != StateDelivered {
		t.Fatalf("live send did not deliver: state=%s reason=%q", res.State, res.Reason)
	}
}
