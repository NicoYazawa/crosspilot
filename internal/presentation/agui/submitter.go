// RunSubmitter 把 application.Bridge 适配到 agui.RunSubmitter 端口。
//
// application 层只暴露 OrchestratorRunner 接口（参数是 RunnerConfig），
// 而 agui.RunSubmitter 接受 SubmitRequest（更窄）。submitAdapter 在此处
// 做窄到窄的字段映射，避免 application 层被迫知道 HTTP 形态。

package agui

import (
	"context"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	"github.com/NicoYazawa/crosspilot/internal/application/orderflow"
)

// RunnerBackend 是 application 层的 OrchestratorRunner 接口的别名（仅供本包复用）。
type RunnerBackend interface {
	Run(ctx context.Context, cfg orderflow.RunnerConfig) ([]runevent.Event, error)
}

// NewSubmitter 把 application OrchestratorRunner 包成 agui.RunSubmitter。
func NewSubmitter(backend RunnerBackend, defaultAgent string) RunSubmitter {
	return &submitAdapter{backend: backend, defaultAgent: defaultAgent}
}

type submitAdapter struct {
	backend     RunnerBackend
	defaultAgent string
}

func (s *submitAdapter) Submit(ctx context.Context, req SubmitRequest) ([]runevent.Event, error) {
	cfg := orderflow.RunnerConfig{
		RunID:     req.RunID,
		BuyerID:   req.BuyerID,
		SessionID: req.SessionID,
		Query:     req.Query,
		Agent:     req.Agent,
	}
	if cfg.Agent == "" {
		cfg.Agent = s.defaultAgent
	}
	return s.backend.Run(ctx, cfg)
}