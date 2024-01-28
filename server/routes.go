package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"github.com/jmorganca/ollama/api"
	"github.com/jmorganca/ollama/gpu"
	"github.com/jmorganca/ollama/llm"
	"github.com/jmorganca/ollama/openai"
	"github.com/jmorganca/ollama/parser"
	"github.com/jmorganca/ollama/version"
)

var mode string = gin.DebugMode

type Server struct {
	WorkDir string
}

func init() {
	switch mode {
	case gin.DebugMode:
	case gin.ReleaseMode:
	case gin.TestMode:
	default:
		mode = gin.DebugMode
	}

	gin.SetMode(mode)
}

var loaded struct {
	mu sync.Mutex

	runner llm.LLM

	expireAt    time.Time
	expireTimer *time.Timer

	*Model
	*api.Options
}

var defaultSessionDuration = 5 * time.Minute

// load a model into memory if it is not already loaded, it is up to the caller to lock loaded.mu before calling this function
func load(model *Model, opts api.Options, sessionDuration time.Duration) error {
	needLoad := loaded.runner == nil || // is there a model loaded?
		loaded.ModelPath != model.ModelPath || // has the base model changed?
		!reflect.DeepEqual(loaded.AdapterPaths, model.AdapterPaths) || // have the adapters changed?
		!reflect.DeepEqual(loaded.Options.Runner, opts.Runner) // have the runner options changed?

	if needLoad {
		if loaded.runner != nil {
			slog.Info("changing loaded model")
			loaded.runner.Close()
			loaded.runner = nil
			loaded.Model = nil
			loaded.Options = nil
		}

		llmRunner, err := llm.New(model.ModelPath, model.AdapterPaths, model.ProjectorPaths, opts)
		if err != nil {
			// some older models are not compatible with newer versions of llama.cpp
			// show a generalized compatibility error until there is a better way to
			// check for model compatibility
			if errors.Is(llm.ErrUnsupportedFormat, err) || strings.Contains(err.Error(), "failed to load model") {
				err = fmt.Errorf("%v: this model may be incompatible with your version of Ollama. If you previously pulled this model, try updating it by running `ollama pull %s`", err, model.ShortName)
			}

			return err
		}

		loaded.Model = model
		loaded.runner = llmRunner
		loaded.Options = &opts
	}

	loaded.expireAt = time.Now().Add(sessionDuration)

	if loaded.expireTimer == nil {
		loaded.expireTimer = time.AfterFunc(sessionDuration, func() {
			loaded.mu.Lock()
			defer loaded.mu.Unlock()

			if time.Now().Before(loaded.expireAt) {
				return
			}

			if loaded.runner != nil {
				loaded.runner.Close()
			}

			loaded.runner = nil
			loaded.Model = nil
			loaded.Options = nil
		})
	}

	loaded.expireTimer.Reset(sessionDuration)
	return nil
}

func modelOptions(model *Model, requestOpts map[string]interface{}) (api.Options, error) {
	opts := api.DefaultOptions()
	if err := opts.FromMap(model.Options); err != nil {
		return api.Options{}, err
	}

	if err := opts.FromMap(requestOpts); err != nil {
		return api.Options{}, err
	}

	return opts, nil
}

func GenerateHandler(c *gin.Context) {
	loaded.mu.Lock()
	defer loaded.mu.Unlock()

	checkpointStart := time.Now()
	var req api.GenerateRequest
	err := c.ShouldBindJSON(&req)

	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// validate the request
	switch {
	case req.Model == "":
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	case len(req.Format) > 0 && req.Format != "json":
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "format must be json"})
		return
	case req.Raw && (req.Template != "" || req.System != "" || len(req.Context) > 0):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "raw mode does not support template, system, or context"})
		return
	}

	model, err := GetModel(req.Model)
	if err != nil {
		var pErr *fs.PathError
		if errors.As(err, &pErr) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found, try pulling it first", req.Model)})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	opts, err := modelOptions(model, req.Options)
	if err != nil {
		if errors.Is(err, api.ErrInvalidOpts) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var sessionDuration time.Duration
	if req.KeepAlive == nil {
		sessionDuration = defaultSessionDuration
	} else {
		sessionDuration = req.KeepAlive.Duration
	}

	if err := load(model, opts, sessionDuration); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// an empty request loads the model
	if req.Prompt == "" && req.Template == "" && req.System == "" {
		c.JSON(http.StatusOK, api.GenerateResponse{
			CreatedAt: time.Now().UTC(),
			Model:     req.Model,
			Done:      true,
		})
		return
	}

	checkpointLoaded := time.Now()

	var prompt string
	var promptVars PromptVars
	switch {
	case req.Raw:
		prompt = req.Prompt
	case req.Prompt != "":
		if req.Template != "" {
			// override the default model template
			model.Template = req.Template
		}

		var rebuild strings.Builder
		if req.Context != nil {
			// TODO: context is deprecated, at some point the context logic within this conditional should be removed
			prevCtx, err := loaded.runner.Decode(c.Request.Context(), req.Context)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			// Remove leading spaces from prevCtx if present
			prevCtx = strings.TrimPrefix(prevCtx, " ")
			rebuild.WriteString(prevCtx)
		}
		promptVars = PromptVars{
			System: req.System,
			Prompt: req.Prompt,
			First:  len(req.Context) == 0,
		}

		if promptVars.System == "" {
			promptVars.System = model.System
		}

		p, err := model.PreResponsePrompt(promptVars)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		rebuild.WriteString(p)
		prompt = rebuild.String()
	}

	slog.Debug(fmt.Sprintf("prompt: %s", prompt))

	ch := make(chan any)
	var generated strings.Builder
	go func() {
		defer close(ch)

		fn := func(r llm.PredictResult) {
			// Update model expiration
			loaded.expireAt = time.Now().Add(sessionDuration)
			loaded.expireTimer.Reset(sessionDuration)

			// Build up the full response
			if _, err := generated.WriteString(r.Content); err != nil {
				ch <- gin.H{"error": err.Error()}
				return
			}

			resp := api.GenerateResponse{
				Model:     req.Model,
				CreatedAt: time.Now().UTC(),
				Done:      r.Done,
				Response:  r.Content,
				Metrics: api.Metrics{
					PromptEvalCount:    r.PromptEvalCount,
					PromptEvalDuration: r.PromptEvalDuration,
					EvalCount:          r.EvalCount,
					EvalDuration:       r.EvalDuration,
				},
			}

			if r.Done {
				resp.TotalDuration = time.Since(checkpointStart)
				resp.LoadDuration = checkpointLoaded.Sub(checkpointStart)

				if !req.Raw {
					// append the generated text to the history and template it if needed
					promptVars.Response = generated.String()
					result, err := model.PostResponseTemplate(promptVars)
					if err != nil {
						ch <- gin.H{"error": err.Error()}
						return
					}
					embd, err := loaded.runner.Encode(c.Request.Context(), prompt+result)
					if err != nil {
						ch <- gin.H{"error": err.Error()}
						return
					}
					resp.Context = embd
				}
			}

			ch <- resp
		}

		// Start prediction
		predictReq := llm.PredictOpts{
			Prompt:  prompt,
			Format:  req.Format,
			Images:  req.Images,
			Options: opts,
		}
		if err := loaded.runner.Predict(c.Request.Context(), predictReq, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		// Accumulate responses into the final response
		var final api.GenerateResponse
		var sb strings.Builder
		for resp := range ch {
			switch r := resp.(type) {
			case api.GenerateResponse:
				sb.WriteString(r.Response)
				final = r
			case gin.H:
				if errorMsg, ok := r["error"].(string); ok {
					c.JSON(http.StatusInternalServerError, gin.H{"error": errorMsg})
					return
				} else {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected error format in response"})
					return
				}
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected error"})
				return
			}
		}

		final.Response = sb.String()
		c.JSON(http.StatusOK, final)
		return
	}

	streamResponse(c, ch)
}

func EmbeddingHandler(c *gin.Context) {
	loaded.mu.Lock()
	defer loaded.mu.Unlock()

	var req api.EmbeddingRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Model == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	model, err := GetModel(req.Model)
	if err != nil {
		var pErr *fs.PathError
		if errors.As(err, &pErr) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found, try pulling it first", req.Model)})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	opts, err := modelOptions(model, req.Options)
	if err != nil {
		if errors.Is(err, api.ErrInvalidOpts) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var sessionDuration time.Duration
	if req.KeepAlive == nil {
		sessionDuration = defaultSessionDuration
	} else {
		sessionDuration = req.KeepAlive.Duration
	}

	if err := load(model, opts, sessionDuration); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if !loaded.Options.EmbeddingOnly {
		c.JSON(http.StatusBadRequest, gin.H{"error": "embedding option must be set to true"})
		return
	}

	embedding, err := loaded.runner.Embedding(c.Request.Context(), req.Prompt)
	if err != nil {
		slog.Info(fmt.Sprintf("embedding generation failed: %v", err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate embedding"})
		return
	}

	resp := api.EmbeddingResponse{
		Embedding: embedding,
	}
	c.JSON(http.StatusOK, resp)
}

func PullModelHandler(c *gin.Context) {
	var req api.PullRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var model string
	if req.Model != "" {
		model = req.Model
	} else if req.Name != "" {
		model = req.Name
	} else {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(r api.ProgressResponse) {
			ch <- r
		}

		regOpts := &RegistryOptions{
			Insecure: req.Insecure,
		}

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		if err := PullModel(ctx, model, regOpts, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		waitForStream(c, ch)
		return
	}

	streamResponse(c, ch)
}

func PushModelHandler(c *gin.Context) {
	var req api.PushRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var model string
	if req.Model != "" {
		model = req.Model
	} else if req.Name != "" {
		model = req.Name
	} else {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(r api.ProgressResponse) {
			ch <- r
		}

		regOpts := &RegistryOptions{
			Insecure: req.Insecure,
		}

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		if err := PushModel(ctx, model, regOpts, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		waitForStream(c, ch)
		return
	}

	streamResponse(c, ch)
}

func CreateModelHandler(c *gin.Context) {
	var req api.CreateRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var model string
	if req.Model != "" {
		model = req.Model
	} else if req.Name != "" {
		model = req.Name
	} else {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	if err := ParseModelPath(model).Validate(); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Path == "" && req.Modelfile == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "path or modelfile are required"})
		return
	}

	var modelfile io.Reader = strings.NewReader(req.Modelfile)
	if req.Path != "" && req.Modelfile == "" {
		mf, err := os.Open(req.Path)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("error reading modelfile: %s", err)})
			return
		}
		defer mf.Close()

		modelfile = mf
	}

	commands, err := parser.Parse(modelfile)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(resp api.ProgressResponse) {
			ch <- resp
		}

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		if err := CreateModel(ctx, model, filepath.Dir(req.Path), commands, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		waitForStream(c, ch)
		return
	}

	streamResponse(c, ch)
}

func DeleteModelHandler(c *gin.Context) {
	var req api.DeleteRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var model string
	if req.Model != "" {
		model = req.Model
	} else if req.Name != "" {
		model = req.Name
	} else {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	if err := DeleteModel(model); err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found", model)})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	manifestsPath, err := GetManifestPath()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if err := PruneDirectory(manifestsPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, nil)
}

func ShowModelHandler(c *gin.Context) {
	var req api.ShowRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Model != "" {
		// noop
	} else if req.Name != "" {
		req.Model = req.Name
	} else {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	resp, err := GetModelInfo(req)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found", req.Model)})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, resp)
}

func GetModelInfo(req api.ShowRequest) (*api.ShowResponse, error) {
	model, err := GetModel(req.Model)
	if err != nil {
		return nil, err
	}

	modelDetails := api.ModelDetails{
		ParentModel:       model.ParentModel,
		Format:            model.Config.ModelFormat,
		Family:            model.Config.ModelFamily,
		Families:          model.Config.ModelFamilies,
		ParameterSize:     model.Config.ModelType,
		QuantizationLevel: model.Config.FileType,
	}

	if req.System != "" {
		model.System = req.System
	}

	if req.Template != "" {
		model.Template = req.Template
	}

	msgs := make([]api.Message, 0)
	for _, msg := range model.Messages {
		msgs = append(msgs, api.Message{Role: msg.Role, Content: msg.Content})
	}

	resp := &api.ShowResponse{
		License:  strings.Join(model.License, "\n"),
		System:   model.System,
		Template: model.Template,
		Details:  modelDetails,
		Messages: msgs,
	}

	var params []string
	cs := 30
	for k, v := range model.Options {
		switch val := v.(type) {
		case []interface{}:
			for _, nv := range val {
				params = append(params, fmt.Sprintf("%-*s %#v", cs, k, nv))
			}
		default:
			params = append(params, fmt.Sprintf("%-*s %#v", cs, k, v))
		}
	}
	resp.Parameters = strings.Join(params, "\n")

	for k, v := range req.Options {
		if _, ok := req.Options[k]; ok {
			model.Options[k] = v
		}
	}

	mf, err := ShowModelfile(model)
	if err != nil {
		return nil, err
	}

	resp.Modelfile = mf

	return resp, nil
}

func ListModelsHandler(c *gin.Context) {
	models := make([]api.ModelResponse, 0)
	manifestsPath, err := GetManifestPath()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	modelResponse := func(modelName string) (api.ModelResponse, error) {
		model, err := GetModel(modelName)
		if err != nil {
			return api.ModelResponse{}, err
		}

		modelDetails := api.ModelDetails{
			Format:            model.Config.ModelFormat,
			Family:            model.Config.ModelFamily,
			Families:          model.Config.ModelFamilies,
			ParameterSize:     model.Config.ModelType,
			QuantizationLevel: model.Config.FileType,
		}

		return api.ModelResponse{
			Model:   model.ShortName,
			Name:    model.ShortName,
			Size:    model.Size,
			Digest:  model.Digest,
			Details: modelDetails,
		}, nil
	}

	walkFunc := func(path string, info os.FileInfo, _ error) error {
		if !info.IsDir() {
			path, tag := filepath.Split(path)
			model := strings.Trim(strings.TrimPrefix(path, manifestsPath), string(os.PathSeparator))
			modelPath := strings.Join([]string{model, tag}, ":")
			canonicalModelPath := strings.ReplaceAll(modelPath, string(os.PathSeparator), "/")

			resp, err := modelResponse(canonicalModelPath)
			if err != nil {
				slog.Info(fmt.Sprintf("skipping file: %s", canonicalModelPath))
				// nolint: nilerr
				return nil
			}

			resp.ModifiedAt = info.ModTime()
			models = append(models, resp)
		}

		return nil
	}

	if err := filepath.Walk(manifestsPath, walkFunc); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, api.ListResponse{Models: models})
}

func CopyModelHandler(c *gin.Context) {
	var req api.CopyRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Source == "" || req.Destination == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "source add destination are required"})
		return
	}

	if err := ParseModelPath(req.Destination).Validate(); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := CopyModel(req.Source, req.Destination); err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found", req.Source)})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
}

func HeadBlobHandler(c *gin.Context) {
	path, err := GetBlobsPath(c.Param("digest"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if _, err := os.Stat(path); err != nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("blob %q not found", c.Param("digest"))})
		return
	}

	c.Status(http.StatusOK)
}

func CreateBlobHandler(c *gin.Context) {
	layer, err := NewLayer(c.Request.Body, "")
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if layer.Digest != c.Param("digest") {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("digest mismatch, expected %q, got %q", c.Param("digest"), layer.Digest)})
		return
	}

	if _, err := layer.Commit(); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.Status(http.StatusCreated)
}

var defaultAllowOrigins = []string{
	"localhost",
	"127.0.0.1",
	"0.0.0.0",
}

func NewServer() (*Server, error) {
	workDir, err := os.MkdirTemp("", "ollama")
	if err != nil {
		return nil, err
	}

	return &Server{
		WorkDir: workDir,
	}, nil
}

func (s *Server) GenerateRoutes() http.Handler {
	var origins []string
	if o := os.Getenv("OLLAMA_ORIGINS"); o != "" {
		origins = strings.Split(o, ",")
	}

	config := cors.DefaultConfig()
	config.AllowWildcard = true
	config.AllowBrowserExtensions = true

	config.AllowOrigins = origins
	for _, allowOrigin := range defaultAllowOrigins {
		config.AllowOrigins = append(config.AllowOrigins,
			fmt.Sprintf("http://%s", allowOrigin),
			fmt.Sprintf("https://%s", allowOrigin),
			fmt.Sprintf("http://%s:*", allowOrigin),
			fmt.Sprintf("https://%s:*", allowOrigin),
		)
	}

	r := gin.Default()
	r.Use(
		cors.New(config),
		func(c *gin.Context) {
			c.Set("workDir", s.WorkDir)
			c.Next()
		},
	)

	r.POST("/api/pull", PullModelHandler)
	r.POST("/api/generate", GenerateHandler)
	r.POST("/api/chat", ChatHandler)
	r.POST("/api/embeddings", EmbeddingHandler)
	r.POST("/api/create", CreateModelHandler)
	r.POST("/api/push", PushModelHandler)
	r.POST("/api/copy", CopyModelHandler)
	r.DELETE("/api/delete", DeleteModelHandler)
	r.POST("/api/show", ShowModelHandler)
	r.POST("/api/blobs/:digest", CreateBlobHandler)
	r.HEAD("/api/blobs/:digest", HeadBlobHandler)

	// Experimental compatibility endpoints
	r.POST("/v1/chat/completions", ChatCompletionsHandler)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r.Handle(method, "/", func(c *gin.Context) {
			c.String(http.StatusOK, "Ollama is running")
		})

		r.Handle(method, "/api/tags", ListModelsHandler)
		r.Handle(method, "/api/version", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"version": version.Version})
		})
	}

	return r
}

func Serve(ln net.Listener) error {
	if debug := os.Getenv("OLLAMA_DEBUG"); debug != "" {
		var programLevel = new(slog.LevelVar)
		h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: programLevel, AddSource: true})
		slog.SetDefault(slog.New(h))
		programLevel.Set(slog.LevelDebug)
		slog.Debug("Debug logging enabled")
	}
	if noprune := os.Getenv("OLLAMA_NOPRUNE"); noprune == "" {
		// clean up unused layers and manifests
		if err := PruneLayers(); err != nil {
			return err
		}

		manifestsPath, err := GetManifestPath()
		if err != nil {
			return err
		}

		if err := PruneDirectory(manifestsPath); err != nil {
			return err
		}
	}

	s, err := NewServer()
	if err != nil {
		return err
	}
	r := s.GenerateRoutes()

	slog.Info(fmt.Sprintf("Listening on %s (version %s)", ln.Addr(), version.Version))
	srvr := &http.Server{
		Handler: r,
	}

	// listen for a ctrl+c and stop any loaded llm
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		if loaded.runner != nil {
			loaded.runner.Close()
		}
		os.RemoveAll(s.WorkDir)
		os.Exit(0)
	}()

	if err := llm.Init(s.WorkDir); err != nil {
		return fmt.Errorf("unable to initialize llm library %w", err)
	}
	if runtime.GOOS == "linux" { // TODO - windows too
		// check compatibility to log warnings
		if _, err := gpu.CheckVRAM(); err != nil {
			slog.Info(err.Error())
		}
	}

	return srvr.Serve(ln)
}

func waitForStream(c *gin.Context, ch chan interface{}) {
	c.Header("Content-Type", "application/json")
	for resp := range ch {
		switch r := resp.(type) {
		case api.ProgressResponse:
			if r.Status == "success" {
				c.JSON(http.StatusOK, r)
				return
			}
		case gin.H:
			if errorMsg, ok := r["error"].(string); ok {
				c.JSON(http.StatusInternalServerError, gin.H{"error": errorMsg})
				return
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected error format in progress response"})
				return
			}
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected progress response"})
			return
		}
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected end of progress response"})
}

func streamResponse(c *gin.Context, ch chan any) {
	c.Header("Content-Type", "application/x-ndjson")
	c.Stream(func(w io.Writer) bool {
		val, ok := <-ch
		if !ok {
			return false
		}

		bts, err := json.Marshal(val)
		if err != nil {
			slog.Info(fmt.Sprintf("streamResponse: json.Marshal failed with %s", err))
			return false
		}

		// Delineate chunks with new-line delimiter
		bts = append(bts, '\n')
		if _, err := w.Write(bts); err != nil {
			slog.Info(fmt.Sprintf("streamResponse: w.Write failed with %s", err))
			return false
		}

		return true
	})
}

func ChatHandler(c *gin.Context) {
	var req api.ChatRequest
	err := c.ShouldBindJSON(&req)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ch, err := chat(c.Request.Context(), req)
	if err != nil {
		var se api.StatusError
		switch {
		case errors.As(err, &se):
			c.AbortWithStatusJSON(se.StatusCode, se)
		default:
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
	}

	stream := req.Stream == nil || *req.Stream && len(req.Messages) > 0
	if stream {
		streamResponse(c, ch)
		return
	}

	var content string
	var res api.ChatResponse
	for r := range ch {
		switch r := r.(type) {
		case api.ChatResponse:
			content += r.Message.Content
			res = r
		case api.StatusError:
			c.AbortWithStatusJSON(r.StatusCode, r)
			return
		}
	}
	res.Message.Content = content
	c.JSON(http.StatusOK, res)
}

func chat(ctx context.Context, req api.ChatRequest) (chan any, error) {
	loaded.mu.Lock()
	defer loaded.mu.Unlock()

	checkpointStart := time.Now()

	if req.Model == "" {
		return nil, api.StatusError{
			StatusCode:   http.StatusBadRequest,
			ErrorMessage: "model is required",
		}
	}

	if len(req.Format) > 0 && req.Format != "json" {
		return nil, api.StatusError{
			StatusCode:   http.StatusBadRequest,
			ErrorMessage: "format must be json",
		}
	}

	model, err := GetModel(req.Model)
	if err != nil {
		var pErr *fs.PathError
		if errors.As(err, &pErr) {
			return nil, api.StatusError{
				StatusCode:   http.StatusNotFound,
				ErrorMessage: fmt.Sprintf("model '%s' not found, try pulling it first", req.Model),
			}
		}
		return nil, api.StatusError{
			StatusCode:   http.StatusInternalServerError,
			ErrorMessage: err.Error(),
		}
	}

	opts, err := modelOptions(model, req.Options)
	if err != nil {
		if errors.Is(err, api.ErrInvalidOpts) {
			return nil, api.StatusError{
				StatusCode:   http.StatusBadRequest,
				ErrorMessage: err.Error(),
			}
		}
		return nil, api.StatusError{
			StatusCode:   http.StatusInternalServerError,
			ErrorMessage: err.Error(),
		}
	}

	sessionDuration := defaultSessionDuration
	if req.KeepAlive != nil {
		sessionDuration = req.KeepAlive.Duration
	}

	if err := load(model, opts, sessionDuration); err != nil {
		return nil, api.StatusError{
			StatusCode:   http.StatusInternalServerError,
			ErrorMessage: err.Error(),
		}
	}

	checkpointLoaded := time.Now()

	prompt, images, err := model.ChatPrompt(req.Messages)
	if err != nil {
		return nil, api.StatusError{
			StatusCode:   http.StatusBadRequest,
			ErrorMessage: err.Error(),
		}
	}

	slog.Debug(fmt.Sprintf("prompt: %s", prompt))

	ch := make(chan any)

	go func() {
		// an empty request loads the model
		if len(req.Messages) == 0 {
			resp := api.ChatResponse{
				CreatedAt: time.Now().UTC(),
				Model:     req.Model,
				Done:      true,
				Message:   api.Message{Role: "assistant"},
			}
			ch <- resp
			return
		}

		fn := func(r llm.PredictResult) {
			loaded.expireAt = time.Now().Add(sessionDuration)
			loaded.expireTimer.Reset(sessionDuration)

			resp := api.ChatResponse{
				Model:     req.Model,
				CreatedAt: time.Now().UTC(),
				Message:   api.Message{Role: "assistant", Content: r.Content},
				Done:      r.Done,
				Metrics: api.Metrics{
					PromptEvalCount:    r.PromptEvalCount,
					PromptEvalDuration: r.PromptEvalDuration,
					EvalCount:          r.EvalCount,
					EvalDuration:       r.EvalDuration,
				},
			}

			if r.Done {
				resp.TotalDuration = time.Since(checkpointStart)
				resp.LoadDuration = checkpointLoaded.Sub(checkpointStart)
			}

			ch <- resp
		}

		predictReq := llm.PredictOpts{
			Prompt:  prompt,
			Format:  req.Format,
			Images:  images,
			Options: opts,
		}

		err := loaded.runner.Predict(ctx, predictReq, fn)
		if err != nil {
			// TODO: return this error outside of the goroutine
			ch <- api.StatusError{
				StatusCode:   http.StatusInternalServerError,
				ErrorMessage: err.Error(),
			}
		}

		close(ch)
	}()

	return ch, nil
}

func ChatCompletionsHandler(c *gin.Context) {
	var req openai.Request
	err := c.ShouldBindJSON(&req)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var messages []api.Message
	for _, msg := range req.Messages {
		messages = append(messages, api.Message{Role: msg.Role, Content: msg.Content})
	}

	options := make(map[string]interface{})
	if req.Seed != nil {
		options["seed"] = req.Seed
	}

	switch s := req.Stop.(type) {
	case string:
		options["stop"] = []string{s}
	case []string:
		options["stop"] = s
	}

	if req.MaxTokens != nil {
		options["num_predict"] = *req.MaxTokens
	}

	if req.Temperature != nil {
		options["temperature"] = *req.Temperature
	}

	if req.FrequencyPenalty != nil {
		options["frequency_penalty"] = (*req.FrequencyPenalty + 2.0) / 4.0
	}

	if req.PresencePenalty != nil {
		options["presence_penalty"] = (*req.PresencePenalty + 2.0) / 4.0
	}

	if req.TopP != nil {
		options["top_p"] = *req.TopP
	}

	var format string
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object" {
		format = "json"
	}

	ch, err := chat(c.Request.Context(), api.ChatRequest{
		Model:    req.Model,
		Messages: messages,
		Format:   format,
		Options:  options,
	})
	if err != nil {
		var se api.StatusError
		switch {
		case errors.As(err, &se):
			c.AbortWithStatusJSON(se.StatusCode, se)
		default:
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}

		return
	}

	if req.Stream {
		streamEvents(c, ch)
		return
	}

	id := fmt.Sprintf("chatcmpl-%d", rand.Intn(999))
	var content string
	var res api.ChatResponse
	for r := range ch {
		switch r := r.(type) {
		case api.ChatResponse:
			content += r.Message.Content
			res = r
		case api.StatusError:
			c.AbortWithStatusJSON(r.StatusCode, r)
			return
		}
	}

	res.Message.Content = content
	c.JSON(http.StatusOK, &openai.Response{
		Id:                id,
		Object:            "chat.completion",
		Created:           res.CreatedAt.Unix(),
		Model:             req.Model,
		SystemFingerprint: "fp_ollama",
		Choices: []openai.Choice{{
			Index:   0,
			Message: openai.Message{Role: res.Message.Role, Content: res.Message.Content},
			FinishReason: func(done bool) *string {
				if done {
					reason := "stop"
					return &reason
				}
				return nil
			}(res.Done),
		}},
		Usage: openai.Usage{
			// TODO: ollama returns 0 for prompt eval if the prompt was cached, but openai returns the actual count
			PromptTokens:     res.PromptEvalCount,
			CompletionTokens: res.EvalCount,
			TotalTokens:      res.PromptEvalCount + res.EvalCount,
		},
	})
}

func streamEvents(c *gin.Context, ch chan any) {
	id := fmt.Sprintf("chatcmpl-%d", rand.Intn(999))
	c.Header("Content-Type", "text/event-stream")
	c.Stream(func(w io.Writer) bool {
		r, ok := <-ch
		if !ok {
			// send "data: [DONE]" as last event
			if _, err := w.Write([]byte("data: [DONE]")); err != nil {
				log.Printf("streamEvents: write failed: %s", err)
			}
			return false
		}

		var chunk openai.Chunk
		switch r := r.(type) {
		case api.ChatResponse:
			chunk = openai.Chunk{
				Id:                id,
				Object:            "chat.completion.chunk",
				Created:           time.Now().Unix(),
				Model:             r.Model,
				SystemFingerprint: "fp_ollama",
				Choices: []openai.ChunkChoice{
					{
						Index: 0,
						Delta: openai.Message{Role: "assistant", Content: r.Message.Content},
					},
				},
			}
		}

		bts, err := json.Marshal(chunk)
		if err != nil {
			log.Printf("streamEvents: marshal failed: %s", err)
			return false
		}

		if _, err := w.Write([]byte(fmt.Sprintf("data: %s\n\n", bts))); err != nil {
			log.Printf("streamEvents: write failed %s", err)
			return false
		}

		return true
	})
}
