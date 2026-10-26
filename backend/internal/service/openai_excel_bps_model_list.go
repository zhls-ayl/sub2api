package service

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// A BPS-only account serves exactly what its model restriction
// (credentials.model_mapping) names, or ExcelBPSDefaultModels while it is
// empty. Test pickers, model listings and Codex manifests are built from that
// configuration alone; BPS model discovery only backs "sync upstream models".

// ExcelBPSConfiguredModels lists the public model names a BPS-only account
// serves: its mapping keys (sorted), or the default models. Other account
// types get an empty list.
func ExcelBPSConfiguredModels(account *Account) []openai.Model {
	ids := excelBPSConfiguredModelIDs(account)
	models := make([]openai.Model, 0, len(ids))
	for _, id := range ids {
		model := openai.Model{ID: id}
		fillExcelBPSModelFields(&model)
		models = append(models, model)
	}
	return models
}

func excelBPSConfiguredModelIDs(account *Account) []string {
	if !account.IsBPSOAuth() {
		return nil
	}
	mapping := account.GetModelMapping()
	if len(mapping) == 0 {
		return append([]string(nil), ExcelBPSDefaultModels...)
	}
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	return sortedExcelBPSModelIDs(keys)
}

// excelBPSUpstreamModelIDs lists the BPS model IDs a BPS-only account sends
// upstream: its mapping targets, or the default models. Raw catalogs start from
// these IDs and are projected through the account mapping like any other.
func excelBPSUpstreamModelIDs(account *Account) []string {
	if !account.IsBPSOAuth() {
		return nil
	}
	mapping := account.GetModelMapping()
	if len(mapping) == 0 {
		return append([]string(nil), ExcelBPSDefaultModels...)
	}
	targets := make([]string, 0, len(mapping))
	for _, target := range mapping {
		targets = append(targets, target)
	}
	return sortedExcelBPSModelIDs(targets)
}

func sortedExcelBPSModelIDs(candidates []string) []string {
	seen := make(map[string]struct{}, len(candidates))
	ids := make([]string, 0, len(candidates))
	for _, id := range candidates {
		id = strings.TrimSpace(id)
		if id == "" || strings.Contains(id, "*") {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// excelBPSConfiguredModelsBody is a BPS-only account's raw model catalog in the
// OpenAI list format, built locally from excelBPSUpstreamModelIDs.
func excelBPSConfiguredModelsBody(account *Account) []byte {
	ids := excelBPSUpstreamModelIDs(account)
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{
			"id": id, "object": "model", "owned_by": "openai", "created": 0,
			"display_name": openaiCodexDisplayName(id),
		})
	}
	body, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	return body
}

func fillExcelBPSModelFields(model *openai.Model) {
	if strings.TrimSpace(model.DisplayName) == "" {
		model.DisplayName = openaiCodexDisplayName(model.ID)
	}
	if strings.TrimSpace(model.Type) == "" {
		model.Type = "model"
	}
	if strings.TrimSpace(model.Object) == "" {
		model.Object = "model"
	}
}
