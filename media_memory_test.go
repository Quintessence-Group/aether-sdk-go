package aether

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mediaString(value string) *string { return &value }

func TestClientRememberMediaUsesMediaRoute(t *testing.T) {
	const entityID = "patient-john"
	const partition = "care-team"
	const createdAt = "2026-07-09T12:00:00Z"
	media := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

	var got rememberMediaRequest
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/memory/media" {
			t.Errorf("path = %q, want /v1/memory/media", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		if r.URL.Query().Get("entity_id") != entityID {
			t.Errorf("entity_id = %q, want %q", r.URL.Query().Get("entity_id"), entityID)
		}
		if r.URL.Query().Get("partition") != partition {
			t.Errorf("partition = %q, want %q", r.URL.Query().Get("partition"), partition)
		}
		if r.URL.Query().Get("readers") != "clinician-1,clinician-2" {
			t.Errorf("readers = %q", r.URL.Query().Get("readers"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(MediaMemoryRecord{
			DocID:       "media-1",
			CID:         "cid-media",
			Modality:    "image",
			ContentType: "image/png",
			DerivedText: "A red bicycle.",
			DerivedBy:   "client",
			CreatedAt:   mediaString(createdAt),
			EntityID:    mediaString(entityID),
			Partition:   mediaString(partition),
			Metadata:    Metadata{"album": "commute"},
		})
	})

	scoped, err := client.Partition(partition)
	if err != nil {
		t.Fatal(err)
	}
	record, err := scoped.RememberMedia(context.Background(), media, MediaMemoryOptions{
		Modality:    "IMAGE",
		ContentType: "image/png; charset=binary",
		EntityID:    entityID,
		Filename:    "bike.png",
		Caption:     "A red bicycle.",
		Tags:        []string{"album:commute"},
		Metadata:    Metadata{"album": "commute"},
		Source:      "camera",
		Readers:     []string{"clinician-1", "clinician-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.DocID != "media-1" || record.DerivedText != "A red bicycle." || record.Modality != "image" {
		t.Errorf("record = %+v", record)
	}
	if got.Modality != "image" || got.ContentType != "image/png" || got.Filename != "bike.png" {
		t.Errorf("wire metadata = %+v", got)
	}
	if got.Caption == nil || *got.Caption != "A red bicycle." {
		t.Errorf("caption = %v", got.Caption)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "album:commute" {
		t.Errorf("tags = %#v", got.Tags)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.DataBase64)
	if err != nil || string(decoded) != string(media) {
		t.Errorf("data_base64 did not round-trip: %q, %v", got.DataBase64, err)
	}
}

func TestClientRememberMediaRejectsInvalidTextBeforeHTTP(t *testing.T) {
	calls := 0
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
	})

	_, err := client.RememberMedia(context.Background(), []byte{1}, MediaMemoryOptions{
		Modality:    "audio",
		ContentType: "audio/wav",
		EntityID:    "patient-john",
		Caption:     "not valid for audio",
	})
	if err == nil || !strings.Contains(err.Error(), "caption is valid only for image") {
		t.Fatalf("err = %v, want caption validation error", err)
	}
	if calls != 0 {
		t.Fatalf("made %d HTTP calls after client-side validation error", calls)
	}
}

func TestMemoryRememberImageAcceptsBytesPathAndURL(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	asset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("media host received Authorization header %q", got)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	t.Cleanup(asset.Close)

	type observedRequest struct {
		contentType string
		filename    string
		caption     string
		data        []byte
	}
	var observed []observedRequest
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/memory/media" {
			t.Errorf("path = %q, want media route", r.URL.Path)
		}
		var body rememberMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode media body: %v", err)
		}
		data, err := base64.StdEncoding.DecodeString(body.DataBase64)
		if err != nil {
			t.Errorf("decode media data: %v", err)
		}
		caption := ""
		if body.Caption != nil {
			caption = *body.Caption
		}
		observed = append(observed, observedRequest{
			contentType: body.ContentType,
			filename:    body.Filename,
			caption:     caption,
			data:        data,
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(MediaMemoryRecord{
			DocID:       "media-" + caption,
			Modality:    "image",
			ContentType: body.ContentType,
			DerivedText: caption,
			DerivedBy:   "client",
			EntityID:    mediaString("patient-john"),
			Metadata:    body.Metadata,
		})
	}))
	t.Cleanup(api.Close)

	client := NewClient(api.URL, WithAPIKey("aether-secret-not-for-media-host"))
	mem, err := NewMemoryWithClient("patient-john", client)
	if err != nil {
		t.Fatal(err)
	}

	byBytes, err := mem.RememberImage(context.Background(), png,
		WithMediaCaption("bytes"),
		WithMediaContentType("image/png"),
		WithMediaMetadata(Metadata{"album": "commute"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "photo.png")
	if err := os.WriteFile(path, png, 0o600); err != nil {
		t.Fatal(err)
	}
	byPath, err := mem.RememberImage(context.Background(), path, WithMediaCaption("path"))
	if err != nil {
		t.Fatal(err)
	}
	byURL, err := mem.RememberImage(context.Background(), asset.URL+"/remote.png", WithMediaCaption("url"))
	if err != nil {
		t.Fatal(err)
	}

	for _, item := range []*MemoryItem{byBytes, byPath, byURL} {
		if item.Modality == nil || *item.Modality != "image" {
			t.Errorf("item modality = %v, want image", item.Modality)
		}
	}
	if len(observed) != 3 {
		t.Fatalf("got %d media requests, want 3", len(observed))
	}
	if observed[1].filename != "photo.png" || observed[1].contentType != "image/png" {
		t.Errorf("path input = %+v", observed[1])
	}
	if observed[2].filename != "remote.png" || observed[2].contentType != "image/png" {
		t.Errorf("URL input = %+v", observed[2])
	}
	for _, request := range observed {
		if string(request.data) != string(png) {
			t.Errorf("media bytes = %x, want %x", request.data, png)
		}
	}
}

func TestMemoryRememberAudioValidatesTranscriptionChoice(t *testing.T) {
	wav := []byte("RIFF\x00\x00\x00\x00WAVE")
	calls := 0
	_, mem := memoryServer(t, "patient-john", func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body rememberMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.Modality != "audio" || body.Transcript != nil || body.ContentType != "audio/wav" {
			t.Errorf("body = %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(MediaMemoryRecord{
			DocID:       "audio-1",
			Modality:    "audio",
			ContentType: "audio/wav",
			DerivedText: "Session transcript.",
			DerivedBy:   "configured-transcriber",
			EntityID:    mediaString("patient-john"),
		})
	})

	item, err := mem.RememberAudio(context.Background(), wav)
	if err != nil {
		t.Fatal(err)
	}
	if item.Text != "Session transcript." || item.Modality == nil || *item.Modality != "audio" {
		t.Errorf("item = %+v", item)
	}
	_, err = mem.RememberAudio(context.Background(), wav, WithMediaAutoTranscription(false))
	if err == nil || !strings.Contains(err.Error(), "provide a transcript") {
		t.Fatalf("err = %v, want transcript validation error", err)
	}
	if calls != 1 {
		t.Fatalf("made %d calls, want only the successful audio request", calls)
	}
}

func TestRetrieveMediaUsesIndexedPassageWithoutBinaryDownload(t *testing.T) {
	calls := 0
	modality := "image"
	passage := "A red bicycle."
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/v1/search":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{{
				DocID:       "media-1",
				Score:       96,
				ContentType: "image/png",
				Passage:     &passage,
				Modality:    &modality,
			}}})
		case "/v1/documents/media-1/download":
			t.Errorf("Retrieve attempted to download original media bytes")
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	results, err := client.Retrieve(context.Background(), "bicycle", 1)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("made %d requests, want only search", calls)
	}
	if len(results) != 1 || results[0].Content != passage || results[0].Modality == nil || *results[0].Modality != "image" {
		t.Errorf("results = %+v", results)
	}
}

func TestRetrieveMediaFallsBackToDerivedTextMetadata(t *testing.T) {
	calls := 0
	modality := "audio"
	derived := "Session transcript."
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/v1/search":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{{
				DocID:       "audio-1",
				Score:       90,
				ContentType: "audio/wav",
				Modality:    &modality,
			}}})
		case "/v1/documents/audio-1":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(DocumentRecord{
				DocID:       "audio-1",
				ContentType: "audio/wav",
				Modality:    &modality,
				DerivedText: &derived,
			})
		case "/v1/documents/audio-1/download":
			t.Errorf("Retrieve attempted to download original audio bytes")
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	results, err := client.Retrieve(context.Background(), "session", 1)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("made %d requests, want search + metadata Get", calls)
	}
	if len(results) != 1 || results[0].Content != derived {
		t.Errorf("results = %+v", results)
	}
}

func TestMemoryListUsesMediaDerivedTextWithoutBinaryDownload(t *testing.T) {
	const transcript = "Session transcript."
	modality := "audio"
	calls := 0
	_, mem := memoryServer(t, "patient-john", func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/v1/documents":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"documents": []DocumentRecord{{
					DocID:       "audio-1",
					ContentType: "audio/wav",
					Modality:    &modality,
					DerivedText: mediaString(transcript),
					EntityID:    mediaString("patient-john"),
				}},
				"count": 1, "total": 1, "has_more": false,
			})
		case "/v1/documents/audio-1/download":
			t.Errorf("Memory.List attempted to download original audio bytes")
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	items, err := mem.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("made %d requests, want list only", calls)
	}
	if len(items) != 1 || items[0].Text != transcript || items[0].Modality == nil || *items[0].Modality != "audio" {
		t.Errorf("items = %+v", items)
	}
}
