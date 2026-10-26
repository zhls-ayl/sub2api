package fish

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPricePayload(t *testing.T) {
	var body map[string]any
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tools/image-video/price" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body = decodeJSONBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credits":650,"perItemCredits":650,"pricingVersion":"2026-10-13.2"}`))
	}))
	result, err := client.Price(context.Background(), "tok", Identity{}, PriceParams{
		Model: "gpt-image-2", Variant: VariantTextToImage,
		ImageSize: "1024x1024", Quality: "medium", Count: 2, RefImageCount: 1,
	})
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if result.Credits != 650 || result.PricingVersion != "2026-10-13.2" {
		t.Fatalf("PriceResult = %+v", result)
	}
	if body["modelId"] != "gpt-image-2" || body["variantKey"] != "t2i" || body["count"] != float64(2) {
		t.Fatalf("body = %v", body)
	}
	if body["refImageCount"] != float64(1) {
		t.Fatalf("refImageCount = %v", body["refImageCount"])
	}
	params := body["params"].(map[string]any)
	if params["image_size"] != "1024x1024" || params["quality"] != "medium" {
		t.Fatalf("params = %v", params)
	}
}

func TestPricePayloadOmitsQualityWhenEmpty(t *testing.T) {
	var params map[string]any
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeJSONBody(t, r)
		params = body["params"].(map[string]any)
		_, _ = w.Write([]byte(`{"credits":450,"pricingVersion":"v"}`))
	}))
	_, err := client.Price(context.Background(), "tok", Identity{}, PriceParams{
		Model: "nano-banana-2-1", Variant: VariantTextToImage, ImageSize: "1K", Count: 1,
	})
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if _, has := params["quality"]; has {
		t.Fatalf("quality must be omitted for tier models, params = %v", params)
	}
	if params["image_size"] != "1K" {
		t.Fatalf("image_size = %v", params["image_size"])
	}
}

func TestSubmitGenerationPayload(t *testing.T) {
	var body map[string]any
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tools/image-video/generate" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body = decodeJSONBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"grp","status":"generating","results":[{"id":"m1","status":"generating"}]}`))
	}))
	group, err := client.SubmitGeneration(context.Background(), "tok", Identity{}, GenerateRequest{
		Model: "gpt-image-2", Variant: VariantTextToImage, Prompt: "a cat",
		Count: 1, ImageSize: "608x1088", Quality: "medium",
		ExpectedCredits: 650, PricingVersion: "2026-10-13.2",
	})
	if err != nil {
		t.Fatalf("SubmitGeneration: %v", err)
	}
	if group.ID != "grp" || group.Status != "generating" || len(group.Results) != 1 {
		t.Fatalf("TaskGroup = %+v", group)
	}
	// clientRequestId 为 uuid 形态且每次不同
	requestID := body["clientRequestId"].(string)
	if len(strings.Split(requestID, "-")) != 5 {
		t.Fatalf("clientRequestId = %q", requestID)
	}
	if body["expectedCredits"] != float64(650) || body["pricingVersion"] != "2026-10-13.2" {
		t.Fatalf("pricing echo wrong: %v", body)
	}
	if body["promptEnhance"] != false {
		t.Fatalf("promptEnhance must be false")
	}
	params := body["params"].(map[string]any)
	if params["image_size"] != "608x1088" || params["prompt"] != "a cat" {
		t.Fatalf("params = %v", params)
	}
	if _, has := body["refImages"]; has {
		t.Fatalf("refImages must be absent without refs")
	}
}

func TestSubmitGenerationEditEcho(t *testing.T) {
	var body map[string]any
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = decodeJSONBody(t, r)
		_, _ = w.Write([]byte(`{"id":"g","status":"generating","results":[]}`))
	}))
	_, err := client.SubmitGeneration(context.Background(), "tok", Identity{}, GenerateRequest{
		Model: "gpt-image-2", Variant: VariantEdit, Prompt: "edit it",
		Count: 1, ImageSize: "2048x2048", Quality: "medium",
		RefAssets:       []RefAsset{{ObjectKey: "media_gen_ref/x/image.png", Type: "image"}},
		ExpectedCredits: 1820, PricingVersion: "v",
	})
	if err != nil {
		t.Fatalf("SubmitGeneration: %v", err)
	}
	params := body["params"].(map[string]any)
	assets := params["reference_assets"].([]any)
	if len(assets) != 1 {
		t.Fatalf("reference_assets = %v", assets)
	}
	asset := assets[0].(map[string]any)
	if asset["objectKey"] != "media_gen_ref/x/image.png" || asset["type"] != "image" {
		t.Fatalf("asset = %v", asset)
	}
	refImages := body["refImages"].([]any)
	ref := refImages[0].(map[string]any)
	if ref["role"] != "ref" || ref["status"] != "ready" || ref["objectKey"] != "media_gen_ref/x/image.png" {
		t.Fatalf("refImages = %v", refImages)
	}
	refID, _ := ref["id"].(string)
	if len(strings.Split(refID, "-")) != 5 {
		t.Fatalf("refImages[0].id = %q, want uuid", refID)
	}
	if _, hasURL := ref["url"]; hasURL {
		t.Fatalf("refImages must not echo a blob url: %v", ref)
	}
}

func TestPollAndFetchMedia(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/task/group/grp1":
			_, _ = w.Write([]byte(`{"id":"grp1","status":"done","results":[{"id":"m1","status":"done","mediaItem":{"id":"m1","url":"https://r2/x.png"}}]}`))
		case "/tools/image-video/media/m1":
			_, _ = w.Write([]byte(`{"id":"m1","type":"image","url":"https://r2/x.png","objectKey":"tasks/m1/0.png"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	group, err := client.PollTaskGroup(context.Background(), "tok", Identity{}, "grp1")
	if err != nil {
		t.Fatalf("PollTaskGroup: %v", err)
	}
	if group.Status != "done" || group.Results[0].MediaItem.URL != "https://r2/x.png" {
		t.Fatalf("group = %+v", group)
	}
	media, err := client.FetchMedia(context.Background(), "tok", Identity{}, "m1")
	if err != nil {
		t.Fatalf("FetchMedia: %v", err)
	}
	if media.ObjectKey != "tasks/m1/0.png" {
		t.Fatalf("media = %+v", media)
	}
}

func TestUploadRefImageThreeSteps(t *testing.T) {
	pngBytes := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	var uploadedBytes []byte
	var uploadedContentType string
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/task/upload-url":
			body := decodeJSONBody(t, r)
			if body["purpose"] != "media_ref_image" || body["file_name"] != "image.png" {
				t.Errorf("upload-url body = %v", body)
			}
			if body["file_size"] != float64(len(pngBytes)) {
				t.Errorf("file_size = %v", body["file_size"])
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"object_key": "media_gen_ref/2026/x/image.png",
				"upload_url": "http://" + r.Host + "/r2-presigned-put",
			})
		case r.Method == "PUT" && r.URL.Path == "/r2-presigned-put":
			raw, _ := io.ReadAll(r.Body)
			uploadedBytes = raw
			uploadedContentType = r.Header.Get("Content-Type")
			w.WriteHeader(200)
		case r.Method == "POST" && r.URL.Path == "/tools/image-video/media/inspect":
			body := decodeJSONBody(t, r)
			if body["objectKey"] != "media_gen_ref/2026/x/image.png" {
				t.Errorf("inspect body = %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"objectKey": "media_gen_ref/2026/x/image.png",
				"kind": "image", "width": 512, "height": 512,
			})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	asset, err := client.UploadRefImage(context.Background(), "tok", Identity{}, "image.png", "image/png", pngBytes)
	if err != nil {
		t.Fatalf("UploadRefImage: %v", err)
	}
	if asset.ObjectKey != "media_gen_ref/2026/x/image.png" || asset.Type != "image" {
		t.Fatalf("asset = %+v", asset)
	}
	if string(uploadedBytes) != string(pngBytes) || uploadedContentType != "image/png" {
		t.Fatalf("uploaded %d bytes ct=%s", len(uploadedBytes), uploadedContentType)
	}
}

func TestUserSelfAndWallet(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/self":
			_, _ = w.Write([]byte(`{"_id":"uid-1","email":"a@b.c","nickname":"nick","banned":false}`))
		case "/wallet/uid-1/package":
			_, _ = w.Write([]byte(`{"total":8000,"balance":3420,"type":"free"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	self, err := client.UserSelf(context.Background(), "tok", Identity{})
	if err != nil || self.ID != "uid-1" {
		t.Fatalf("UserSelf: %v %+v", err, self)
	}
	wallet, err := client.QueryWallet(context.Background(), "tok", Identity{}, "uid-1")
	if err != nil || wallet.Balance != 3420 {
		t.Fatalf("QueryWallet: %v %+v", err, wallet)
	}
	// teamID 为空时先 UserSelf 再查钱包
	wallet2, err := client.QueryWallet(context.Background(), "tok", Identity{}, "")
	if err != nil || wallet2.Balance != 3420 {
		t.Fatalf("QueryWallet(auto team): %v %+v", err, wallet2)
	}
}

func TestNewRequestIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newRequestID()
		if len(id) != 36 || strings.Count(id, "-") != 4 {
			t.Fatalf("id shape = %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
