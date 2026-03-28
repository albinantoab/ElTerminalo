package llm

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ModelURL      = "https://huggingface.co/albinab/Qwen-0.5B-Coder-El-Terminalo/resolve/main/Qwen-0.5B-Coder-El-Terminalo-q8.gguf"
	ModelFilename = "Qwen-0.5B-Coder-El-Terminalo-q8.gguf"
	etagFilename  = "Qwen-0.5B-Coder-El-Terminalo-q8.gguf.etag"
)

// ModelPath returns the full path where the model is stored.
func ModelPath(configDir string) string {
	return filepath.Join(configDir, "models", ModelFilename)
}

func etagPath(configDir string) string {
	return filepath.Join(configDir, "models", etagFilename)
}

// ModelExists checks if the model file is already downloaded.
func ModelExists(configDir string) bool {
	info, err := os.Stat(ModelPath(configDir))
	return err == nil && info.Size() > 0
}

// CleanStaleFiles removes partial downloads and old model files from previous versions.
// Only the current ModelFilename and its etag are kept.
func CleanStaleFiles(configDir string) {
	modelsDir := filepath.Join(configDir, "models")
	entries, err := os.ReadDir(modelsDir)
	if err != nil {
		return
	}
	keep := map[string]bool{
		ModelFilename: true,
		etagFilename:  true,
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Remove .download temp files, old model files, old etag files
		if strings.HasSuffix(name, ".download") || !keep[name] {
			os.Remove(filepath.Join(modelsDir, name))
		}
	}
}

// CheckModelUpdate does a lightweight HEAD request to HuggingFace
// and compares the remote ETag with the locally stored one.
// Returns true if a newer model is available.
func CheckModelUpdate(configDir string) bool {
	if !ModelExists(configDir) {
		return false // nothing to update, needs fresh download
	}
	stored, err := os.ReadFile(etagPath(configDir))
	if err != nil {
		return false // no etag stored, can't compare
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Head(ModelURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	remote := remoteETag(resp)
	if remote == "" {
		return false
	}
	return remote != strings.TrimSpace(string(stored))
}

// remoteETag extracts the content ETag from HuggingFace response headers.
// HF uses X-Linked-ETag for LFS files on the redirect, and ETag on the final response.
func remoteETag(resp *http.Response) string {
	if v := resp.Header.Get("X-Linked-ETag"); v != "" {
		return strings.Trim(v, "\"")
	}
	if v := resp.Header.Get("ETag"); v != "" {
		return strings.Trim(v, "\"")
	}
	return ""
}

// httpClient with aggressive timeouts so a network change doesn't hang forever.
var httpClient = &http.Client{
	Timeout: 10 * time.Minute,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	},
}

// DownloadModel downloads the GGUF model from HuggingFace with progress reporting.
// If the model already exists and the remote ETag matches, it's a no-op.
// It stores the remote ETag alongside the model for version checking.
func DownloadModel(ctx context.Context, configDir string, onProgress func(downloaded, total int64)) error {
	destDir := filepath.Join(configDir, "models")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("cannot create models directory: %w", err)
	}

	// If model exists, do a quick HEAD check — skip if ETag matches
	if ModelExists(configDir) {
		if stored, err := os.ReadFile(etagPath(configDir)); err == nil {
			headReq, err := http.NewRequestWithContext(ctx, "HEAD", ModelURL, nil)
			if err != nil {
				return fmt.Errorf("failed to create HEAD request: %w", err)
			}
			if headResp, err := httpClient.Do(headReq); err == nil {
				defer headResp.Body.Close()
				if remote := remoteETag(headResp); remote != "" && remote == strings.TrimSpace(string(stored)) {
					return nil // already up to date
				}
			}
		}
	}

	dest := ModelPath(configDir)
	tmpDest := dest + ".download"

	req, err := http.NewRequestWithContext(ctx, "GET", ModelURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		os.Remove(tmpDest)
		return fmt.Errorf("failed to download model: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	out, err := os.Create(tmpDest)
	if err != nil {
		return fmt.Errorf("cannot create file: %w", err)
	}

	total := resp.ContentLength
	var downloaded int64
	buf := make([]byte, 64*1024)

	for {
		select {
		case <-ctx.Done():
			out.Close()
			os.Remove(tmpDest)
			return ctx.Err()
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				out.Close()
				os.Remove(tmpDest)
				return fmt.Errorf("write error: %w", writeErr)
			}
			downloaded += int64(n)
			if onProgress != nil {
				onProgress(downloaded, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			out.Close()
			os.Remove(tmpDest)
			return fmt.Errorf("download interrupted: %w", readErr)
		}
	}
	out.Close()

	if err := os.Rename(tmpDest, dest); err != nil {
		return err
	}

	// Save the ETag for future update checks
	if etag := remoteETag(resp); etag != "" {
		if err := os.WriteFile(etagPath(configDir), []byte(etag), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to save ETag: %v\n", err)
		}
	}

	return nil
}
