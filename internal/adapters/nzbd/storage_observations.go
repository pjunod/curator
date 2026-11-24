package nzbd

import (
	"context"

	"github.com/pjunod/monarr/internal/ports"
)

func (c *Client) StorageVolumes(ctx context.Context) ([]ports.StorageVolumeObservation, error) {
	status, err := c.status(ctx)
	return status.Storage, err
}
