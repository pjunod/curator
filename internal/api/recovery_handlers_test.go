package api

import (
	"context"
	"strings"
	"testing"
)

func TestRecoverySettingsAreAdvisoryAndCredentialsStayPrivate(t *testing.T) {
	e := newAPIEnv(t)
	e.put(t, "/api/v1/import/recovery/settings", `{"enabled":true,"local_root":"/missing/recovery","remote_root":"/published","consumer_token":"private-recovery-key"}`).expect(t, 200)
	response := e.get(t, "/api/v1/import/recovery/settings").expect(t, 200)
	if strings.Contains(response.Body.String(), "private-recovery-key") {
		t.Fatal("credential leaked")
	}
	if !strings.Contains(response.Body.String(), `"enabled":true`) || !strings.Contains(response.Body.String(), `"local_mount_available":false`) {
		t.Fatalf("advisory=%s", response.Body)
	}
	e.put(t, "/api/v1/import/recovery/settings", `{"enabled":false,"local_root":"/missing/recovery","remote_root":"/published","consumer_token":""}`).expect(t, 200)
	cfg, err := e.acq.RecoverySettings(context.Background())
	if err != nil || cfg.ConsumerToken != "private-recovery-key" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	e.put(t, "/api/v1/import/recovery/settings", `{`).expect(t, 400)
	e.get(t, "/api/v1/import/recovery").expect(t, 200)
	e.get(t, "/api/v1/import/recovery/jobs").expect(t, 200)
}
func TestRecoveryPreviewAdmissionIsDurableAndBadTargetsCannotQueue(t *testing.T) {
	e := newAPIEnv(t)
	e.post(t, "/api/v1/import/recovery/preview", `{`).expect(t, 400)
	e.post(t, "/api/v1/import/recovery", `{`).expect(t, 400)
	body := `{"client_id":99999,"recovery_id":"missing","media_item_id":1,"copy_id":0,"file_ids":[],"target_generation":"","accept_unverified":false}`
	var task struct{ ID, State string }
	e.post(t, "/api/v1/import/recovery/preview", body).expect(t, 202).into(t, &task)
	if task.ID == "" || task.State != "queued" {
		t.Fatalf("task=%+v", task)
	}
	e.get(t, "/api/v1/import/recovery/previews/"+task.ID).expect(t, 200)
	e.get(t, "/api/v1/import/recovery/previews/missing").expect(t, 404)
	e.post(t, "/api/v1/import/recovery", body).expect(t, 409)
	e.get(t, "/api/v1/import/recovery/missing").expect(t, 404)
	e.post(t, "/api/v1/import/recovery/missing/cancel", ``).expect(t, 409)
}
