package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// An older admin frontend may still send the removed excel_bps_enabled
// switch: saving must succeed, ignore the field and not write it back.
func TestUpdateSettingsIgnoresRemovedExcelBPSSwitch(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})

	rec := doUpdateSettings(t, h, map[string]any{"available_channels_enabled": true, "excel_bps_enabled": false}, nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, repo.values, "excel_bps_enabled")
	var resp struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotContains(t, resp.Data, "excel_bps_enabled")
}
