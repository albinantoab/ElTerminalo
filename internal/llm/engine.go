package llm

import (
	"fmt"
	"runtime"
	"strings"
	"sync"

	llama "github.com/AshkanYarmoradi/go-llama.cpp"
)

// Engine wraps the llama.cpp model for in-process inference.
type Engine struct {
	model *llama.LLama
	shell string
	mu    sync.Mutex
}

// NewEngine loads the GGUF model into memory and returns an inference engine.
func NewEngine(modelPath, shell string) (*Engine, error) {
	model, err := llama.New(modelPath,
		llama.SetContext(512),
		llama.SetGPULayers(99), // Offload all layers to GPU (Metal on macOS)
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load model: %w", err)
	}
	return &Engine{model: model, shell: shell}, nil
}

// Generate produces a shell command from a natural language prompt.
func (e *Engine) Generate(prompt, cwd string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	systemPrompt := fmt.Sprintf(
		"You are a shell command generator. OS: %s, Shell: %s, CWD: %s. Output ONLY the command.",
		runtime.GOOS, e.shell, cwd,
	)

	// ChatML format matching the Qwen fine-tuning template
	fullPrompt := fmt.Sprintf(
		"<|im_start|>system\n%s<|im_end|>\n<|im_start|>user\n%s<|im_end|>\n<|im_start|>assistant\n",
		systemPrompt, prompt,
	)

	result, err := e.model.Predict(fullPrompt,
		llama.SetTemperature(0.1),
		llama.SetTopP(0.9),
		llama.SetTokens(256),
		llama.SetStopWords("<|im_start|>", "<|im_end|>"),
		llama.SetThreads(runtime.NumCPU()),
	)
	if err != nil {
		return "", fmt.Errorf("inference failed: %w", err)
	}

	cmd := strings.TrimSpace(result)
	if cmd == "" {
		return "", fmt.Errorf("model returned empty response")
	}
	return cmd, nil
}

// Close frees the model from memory.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.model != nil {
		e.model.Free()
		e.model = nil
	}
}
