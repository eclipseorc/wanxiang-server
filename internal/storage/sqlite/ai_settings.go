package sqlite

import (
	"database/sql"
	"errors"
	"time"
)

// AISettings 持久化视觉大模型连接参数与高危词护栏配置。
type AISettings struct {
	BaseURL    string `json:"baseUrl"`
	APIKey     string `json:"apiKey"`
	Model      string `json:"model"`
	MaxSteps   int    `json:"maxSteps"`
	GuardWords string `json:"guardWords"`
	UpdatedAt  int64  `json:"updatedAt"`
}

func (s *Store) GetAISettings() (AISettings, error) {
	cfg := AISettings{
		MaxSteps:   15,
		GuardWords: "确认付款,立即支付,确认转账,注销账号,删除账号,确认删除",
	}
	err := s.DB.QueryRow(
		`SELECT base_url, api_key, model, max_steps, guard_words, updated_at FROM ai_settings WHERE id=1`,
	).Scan(&cfg.BaseURL, &cfg.APIKey, &cfg.Model, &cfg.MaxSteps, &cfg.GuardWords, &cfg.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 15
	}
	return cfg, err
}

func (s *Store) SaveAISettings(cfg AISettings) error {
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 15
	}
	now := time.Now().Unix()
	_, err := s.DB.Exec(
		`INSERT INTO ai_settings(id, base_url, api_key, model, max_steps, guard_words, updated_at)
		 VALUES(1, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   base_url=excluded.base_url,
		   api_key=CASE WHEN excluded.api_key<>'' THEN excluded.api_key ELSE ai_settings.api_key END,
		   model=excluded.model,
		   max_steps=excluded.max_steps,
		   guard_words=excluded.guard_words,
		   updated_at=excluded.updated_at`,
		cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.MaxSteps, cfg.GuardWords, now,
	)
	return err
}
