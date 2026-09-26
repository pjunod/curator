package acquisition

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

type RecoveryPreviewTask struct {
	ID      string           `json:"id"`
	State   string           `json:"state"`
	Preview *RecoveryPreview `json:"preview,omitempty"`
	Error   string           `json:"error,omitempty"`
}

func (s *Service) StartRecoveryPreview(ctx context.Context, req RecoveryRequest) (RecoveryPreviewTask, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return RecoveryPreviewTask{}, err
	}
	task := RecoveryPreviewTask{ID: hex.EncodeToString(nonce[:]), State: "queued"}
	req.PreviewID = task.ID
	raw, err := json.Marshal(req)
	if err != nil {
		return task, err
	}
	_, err = s.db.W.ExecContext(ctx, `INSERT INTO recovery_previews(id,state,request,updated_at) VALUES(?,?,?,?)`, task.ID, task.State, string(raw), time.Now().UnixMilli())
	return task, err
}
func (s *Service) RecoveryPreviewStatus(ctx context.Context, id string) (RecoveryPreviewTask, error) {
	task := RecoveryPreviewTask{ID: id}
	var raw string
	err := s.db.R.QueryRowContext(ctx, `SELECT state,result,error FROM recovery_previews WHERE id=?`, id).Scan(&task.State, &raw, &task.Error)
	if err != nil {
		return task, err
	}
	if raw != "" {
		err = json.Unmarshal([]byte(raw), &task.Preview)
	}
	return task, err
}
func (s *Service) recoveryPreviewSweep(ctx context.Context) {
	var id, raw string
	if err := s.db.R.QueryRowContext(ctx, `SELECT id,request FROM recovery_previews WHERE state IN ('queued','running') ORDER BY updated_at LIMIT 1`).Scan(&id, &raw); err != nil {
		return
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE recovery_previews SET state='running' WHERE id=?`, id); err != nil {
		return
	}
	var req RecoveryRequest
	err := json.Unmarshal([]byte(raw), &req)
	var preview RecoveryPreview
	if err == nil {
		preview, err = s.PreviewRecovery(ctx, req)
	}
	if ctx.Err() != nil {
		return
	} // Persisted running state resumes after restart.
	state, result, detail := "ready", "", ""
	if err != nil {
		state = "failed"
		detail = err.Error()
	} else {
		bytes, e := json.Marshal(preview)
		if e != nil {
			state = "failed"
			detail = e.Error()
		} else {
			result = string(bytes)
		}
	}
	if _, e := s.db.W.ExecContext(ctx, `UPDATE recovery_previews SET state=?,result=?,error=?,updated_at=? WHERE id=?`, state, result, detail, time.Now().UnixMilli(), id); e != nil {
		s.log.Error("recovery preview remains pending", "id", id, "err", e)
	}
}
func (s *Service) acceptedRecoveryPreview(ctx context.Context, req RecoveryRequest) (RecoveryPreview, error) {
	if req.PreviewID == "" {
		return s.PreviewRecovery(ctx, req)
	}
	task, err := s.RecoveryPreviewStatus(ctx, req.PreviewID)
	if err != nil {
		return RecoveryPreview{}, err
	}
	if task.State != "ready" || task.Preview == nil {
		return RecoveryPreview{}, fmt.Errorf("recovery preview is not ready")
	}
	expected, _ := json.Marshal(task.Preview.Request)
	actual, _ := json.Marshal(req)
	if string(expected) != string(actual) {
		return RecoveryPreview{}, fmt.Errorf("selection differs from the inspected preview")
	}
	return *task.Preview, nil
}
func (s *Service) startRecoveryPreviewWorker(ctx context.Context) {
	s.importWG.Add(1)
	go func() {
		defer s.importWG.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			s.recoveryPreviewSweep(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
