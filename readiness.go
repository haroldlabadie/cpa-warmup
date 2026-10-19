package main

import "context"

// The native host builds management routes from its active capability snapshot.
// A replacement .so has a distinct RPC client, so an old route cannot return
// this instance's nonce. Reconfigure uses the same client; both snapshots still
// invoke our always-enabled before/after interceptor against this Engine.
func (e *Engine) ready(ctx context.Context) bool {
	if e.instance == "" || ctx.Err() != nil {
		return false
	}
	var status struct {
		Nonce    string `json:"instance_nonce"`
		Alive    bool   `json:"worker_alive"`
		Quiesced bool   `json:"quiesced"`
		Enabled  bool   `json:"enabled"`
		Observe  bool   `json:"observe"`
	}
	if e.api.callMethod(ctx, "GET", managementBase+"/status", nil, &status) != nil {
		return false
	}
	return status.Nonce == e.instance && status.Alive && !status.Quiesced && status.Enabled && !status.Observe && ctx.Err() == nil
}
