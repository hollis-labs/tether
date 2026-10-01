package main

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/llm"
)

var (
	aiJSONFlag bool

	aiProviderFlag    string
	aiModelFlag       string
	aiRequestFileFlag string
	aiRequestJSONFlag string
	aiSystemFlag      string
	aiStreamFlag      bool
	aiSessionIDFlag   string
	aiCallerIDFlag    string
	aiRequestIDFlag   string
	aiModeFlag        string
	aiIntentFlag      string
	aiMaxOutputFlag   int
	aiTokenBudgetFlag int
	aiCostBudgetFlag  float64
	aiLatencyTargetMS int
	aiImageFiles      []string
	aiImageURLs       []string

	aiAuditEventTypeFlag string
	aiAuditErrorsOnly    bool
	aiAuditLimit         int
	aiSinceFlag          string
)

var aiClientFactory = defaultAIClientFactory

func defaultAIClientFactory() (*client.Client, error) {
	return newDaemonClient(catalogPath)
}

func aiClient() (*client.Client, error) {
	return aiClientFactory()
}

var aiCmd = &cobra.Command{
	Use:   "ai",
	Short: "AI gateway commands",
}

var aiProvidersCmd = &cobra.Command{
	Use:   "providers",
	Short: "List configured AI providers",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := aiClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.AIProviders(cmdCtx(cmd))
		if err != nil {
			return classifyErr(err)
		}
		if aiJSONFlag {
			return printJSON(out)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tTYPE\tDEFAULT MODEL\tBASE URL")
		for _, p := range out.Providers {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.ID, p.Type, p.DefaultModel, p.BaseURL)
		}
		return w.Flush()
	},
}

var aiModelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List AI models visible to configured providers",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := aiClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.AIModels(cmdCtx(cmd), aiProviderFlag)
		if err != nil {
			return classifyErr(err)
		}
		if aiJSONFlag {
			return printJSON(out)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "CONFIGURED PROVIDER\tMODEL\tFAMILY\tCONTEXT\tMAX OUTPUT\tINPUT\tOUTPUT")
		for _, m := range out.Models {
			fmt.Fprintf(
				w,
				"%s\t%s\t%s\t%d\t%d\t%s\t%s\n",
				m.ConfiguredProviderID,
				m.ID,
				m.Family,
				m.ContextWindow,
				m.MaxOutputTokens,
				strings.Join(m.InputModalities, ","),
				strings.Join(m.OutputModalities, ","),
			)
		}
		return w.Flush()
	},
}

var aiRoutesCmd = &cobra.Command{
	Use:   "routes",
	Short: "List configured AI planner routes",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := aiClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.AIRoutes(cmdCtx(cmd))
		if err != nil {
			return classifyErr(err)
		}
		if aiJSONFlag {
			return printJSON(out)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "PROVIDER\tMODEL\tMODE\tINTENT\tREASONING\tTOOLS\tALLOW REASONING\tALLOW TOOLS\tALLOW ATTACHMENTS\tMAX OUTPUT\tMAX COST USD\tUSAGE BUDGET")
		for _, route := range out.Routes {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				route.Provider,
				route.Model,
				route.Mode,
				route.Intent,
				strconv.FormatBool(route.RequiresReasoning),
				strconv.FormatBool(route.RequiresTools),
				formatOptionalBool(route.AllowReasoning),
				formatOptionalBool(route.AllowTools),
				formatOptionalBool(route.AllowAttachments),
				formatOptionalInt(route.MaxOutputTokens),
				formatOptionalFloat(route.MaxCostUSD),
				formatUsageBudget(route.UsageBudget),
			)
		}
		return w.Flush()
	},
}

var aiChatCmd = &cobra.Command{
	Use:   "chat [text|-]",
	Short: "Send a simple normalized chat request through the AI gateway",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		req, err := buildAIRequest(args)
		if err != nil {
			return err
		}
		c, err := aiClient()
		if err != nil {
			return classifyErr(err)
		}
		if aiStreamFlag {
			stream, errCh, err := c.AIChatStream(cmdCtx(cmd), api.ChatRequest{Request: req})
			if err != nil {
				return classifyErr(err)
			}
			var final *llm.Response
			for {
				select {
				case ev, ok := <-stream:
					if !ok {
						if final != nil && !aiJSONFlag {
							fmt.Println()
							printAIChatSummary(*final)
						}
						return nil
					}
					if aiJSONFlag {
						if err := printJSON(ev); err != nil {
							return err
						}
						continue
					}
					switch ev.Kind {
					case llm.StreamEventStart:
						// no-op: stream start carries no renderable payload
					case llm.StreamEventTextDelta:
						fmt.Print(ev.Delta)
					case llm.StreamEventRefusalDelta:
						fmt.Fprint(os.Stderr, ev.Delta)
					case llm.StreamEventToolUse:
						if ev.ToolUse != nil {
							fmt.Fprintf(os.Stderr, "\ntool_use name=%s invocation=%s arguments=%s\n", ev.ToolUse.Name, ev.ToolUse.Invocation, ev.ToolUse.Arguments)
						}
					case llm.StreamEventError:
						return validationErr("ai: stream error: %s", ev.Error)
					case llm.StreamEventCompleted:
						if ev.Response != nil {
							final = ev.Response
						}
					}
				case err := <-errCh:
					if err != nil {
						return classifyErr(err)
					}
					errCh = nil
				case <-cmdCtx(cmd).Done():
					return nil
				}
			}
		}

		out, err := c.AIChat(cmdCtx(cmd), api.ChatRequest{Request: req})
		if err != nil {
			return classifyErr(err)
		}
		if aiJSONFlag {
			return printJSON(out)
		}
		printAIChatResponse(out.Response)
		return nil
	},
}

var aiEmbeddingsCmd = &cobra.Command{
	Use:   "embeddings [text|-]",
	Short: "Generate embeddings through the AI gateway",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		req, err := buildAIEmbeddingRequest(args)
		if err != nil {
			return err
		}
		c, err := aiClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.AIEmbeddings(cmdCtx(cmd), api.ChatRequest{Request: req})
		if err != nil {
			return classifyErr(err)
		}
		if aiJSONFlag {
			return printJSON(out)
		}
		printAIEmbeddingResponse(out.Response)
		return nil
	},
}

var aiRoutePreviewCmd = &cobra.Command{
	Use:   "route-preview [text|-]",
	Short: "Preview the route the AI planner would choose for a chat request",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		req, err := buildAIRequest(args)
		if err != nil {
			return err
		}
		c, err := aiClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.AIPreviewRoute(cmdCtx(cmd), api.ChatRequest{Request: req})
		if err != nil {
			return classifyErr(err)
		}
		if aiJSONFlag {
			return printJSON(out)
		}
		fmt.Printf("provider: %s\nmodel: %s\nestimated_cost_usd: %.6f\npolicy_version: %s\n",
			out.Route.Provider, out.Route.Model, out.Route.EstimatedCostUSD, out.Route.PolicyVersion)
		if len(out.Route.Reasons) > 0 {
			fmt.Println("reasons:")
			for _, reason := range out.Route.Reasons {
				fmt.Printf("- %s\n", reason)
			}
		}
		return nil
	},
}

var aiRouteExplainCmd = &cobra.Command{
	Use:   "route-explain [text|-]",
	Short: "Explain why each configured AI route matched, failed, or was skipped",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		req, err := buildAIRequest(args)
		if err != nil {
			return err
		}
		c, err := aiClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.AIExplainRoute(cmdCtx(cmd), api.ChatRequest{Request: req})
		if err != nil {
			return classifyErr(err)
		}
		if aiJSONFlag {
			return printJSON(out)
		}
		printAIRouteExplain(out)
		return nil
	},
}

var aiUsageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show durable AI usage aggregates",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAISinceFlag(); err != nil {
			return err
		}
		c, err := aiClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.AIUsage(cmdCtx(cmd), client.AIUsageQuery{
			Provider:  aiProviderFlag,
			Model:     aiModelFlag,
			SessionID: aiSessionIDFlag,
			CallerID:  aiCallerIDFlag,
			Operation: string(llm.OperationChat),
			Since:     aiSinceFlag,
		})
		if err != nil {
			return classifyErr(err)
		}
		if aiJSONFlag {
			return printJSON(out)
		}
		printAIUsage(out)
		return nil
	},
}

var aiBudgetsCmd = &cobra.Command{
	Use:   "budgets",
	Short: "Show live durable AI usage budgets and remaining headroom",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := aiClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.AIBudgets(cmdCtx(cmd), client.AIBudgetsQuery{
			Provider:  aiProviderFlag,
			Model:     aiModelFlag,
			SessionID: aiSessionIDFlag,
			CallerID:  aiCallerIDFlag,
		})
		if err != nil {
			return classifyErr(err)
		}
		if aiJSONFlag {
			return printJSON(out)
		}
		printAIBudgets(out)
		return nil
	},
}

var aiAuditCmd = &cobra.Command{
	Use:   "audit",
	Short: "List durable AI audit events",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAISinceFlag(); err != nil {
			return err
		}
		c, err := aiClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.AIAudit(cmdCtx(cmd), client.AIAuditQuery{
			EventType:  aiAuditEventTypeFlag,
			Provider:   aiProviderFlag,
			Model:      aiModelFlag,
			SessionID:  aiSessionIDFlag,
			CallerID:   aiCallerIDFlag,
			Limit:      aiAuditLimit,
			Since:      aiSinceFlag,
			ErrorsOnly: aiAuditErrorsOnly,
		})
		if err != nil {
			return classifyErr(err)
		}
		if aiJSONFlag {
			return printJSON(out)
		}
		printAIAudit(out)
		return nil
	},
}

var aiWatchBudgetsCmd = &cobra.Command{
	Use:   "watch-budgets",
	Short: "Stream live durable budget rejection events",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := aiClient()
		if err != nil {
			return classifyErr(err)
		}
		stream, errCh, err := c.StreamEvents(cmdCtx(cmd), client.EventsStreamQuery{
			Scopes: []string{events.ScopeDaemon},
			Kinds:  []string{events.KindAIBudgetRejected},
		})
		if err != nil {
			return classifyErr(err)
		}
		for {
			select {
			case ev, ok := <-stream:
				if !ok {
					return nil
				}
				payload, err := parseAIBudgetRejectionPayload(ev.PayloadJSON)
				if err != nil {
					return err
				}
				if !matchesAIBudgetWatchFilters(payload) {
					continue
				}
				if aiJSONFlag {
					if err := printJSON(map[string]any{
						"seq":         ev.Seq,
						"kind":        ev.Kind,
						"scope":       ev.Scope,
						"payload":     payload,
						"payload_raw": ev.PayloadJSON,
					}); err != nil {
						return err
					}
					continue
				}
				fmt.Printf("%d\t%s\t%s\t%s\t%s\n", ev.Seq, payload.Provider, payload.Model, firstNonEmptyString(payload.CallerID, payload.SessionID, "-"), payload.Error)
			case err := <-errCh:
				if err != nil {
					return classifyErr(err)
				}
				errCh = nil
			case <-cmdCtx(cmd).Done():
				return nil
			}
		}
	},
}

func buildAIRequest(args []string) (llm.Request, error) {
	if strings.TrimSpace(aiRequestFileFlag) != "" && strings.TrimSpace(aiRequestJSONFlag) != "" {
		return llm.Request{}, validationErr("ai: --request-file and --request-json are mutually exclusive")
	}
	parts, err := buildAIUserParts(args)
	if err != nil {
		return llm.Request{}, err
	}
	if strings.TrimSpace(aiRequestFileFlag) != "" || strings.TrimSpace(aiRequestJSONFlag) != "" {
		if len(args) > 0 {
			return llm.Request{}, validationErr("ai: positional text cannot be combined with --request-file or --request-json")
		}
		req, err := readAIRequestOverride()
		if err != nil {
			return llm.Request{}, err
		}
		req = applyAIRequestOverrides(req)
		if req.Operation == "" {
			req.Operation = llm.OperationChat
		}
		req = appendAIUserParts(req, parts)
		if len(req.Input) == 0 {
			return llm.Request{}, validationErr("ai: request input is required")
		}
		return req, nil
	}

	req := applyAIRequestOverrides(llm.Request{
		Operation: llm.OperationChat,
		Input:     make([]llm.Message, 0, 2),
	})
	if strings.TrimSpace(aiSystemFlag) != "" {
		req.Input = append(req.Input, llm.Message{
			Role:  "system",
			Parts: []llm.ContentPart{{Type: "text", Text: aiSystemFlag}},
		})
	}
	if len(parts) == 0 {
		return llm.Request{}, validationErr("ai: request text or image input is required")
	}
	req.Input = append(req.Input, llm.Message{Role: "user", Parts: parts})
	return req, nil
}

func buildAIEmbeddingRequest(args []string) (llm.Request, error) {
	if strings.TrimSpace(aiRequestFileFlag) != "" && strings.TrimSpace(aiRequestJSONFlag) != "" {
		return llm.Request{}, validationErr("ai: --request-file and --request-json are mutually exclusive")
	}
	if strings.TrimSpace(aiRequestFileFlag) != "" || strings.TrimSpace(aiRequestJSONFlag) != "" {
		if len(args) > 0 {
			return llm.Request{}, validationErr("ai: positional text cannot be combined with --request-file or --request-json")
		}
		req, err := readAIRequestOverride()
		if err != nil {
			return llm.Request{}, err
		}
		req = applyAIRequestOverrides(req)
		if req.Operation == "" {
			req.Operation = llm.OperationEmbedding
		}
		if req.Operation != llm.OperationEmbedding {
			return llm.Request{}, validationErr("ai: request operation must be embedding")
		}
		if len(req.EmbeddingInput) == 0 {
			return llm.Request{}, validationErr("ai: embedding_input is required")
		}
		return req, nil
	}

	text, err := readAITextArg(args)
	if err != nil {
		return llm.Request{}, validationErr("ai: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		return llm.Request{}, validationErr("ai: embedding text is required")
	}
	req := applyAIRequestOverrides(llm.Request{
		Operation:      llm.OperationEmbedding,
		EmbeddingInput: []string{text},
	})
	return req, nil
}

func readAIRequestOverride() (llm.Request, error) {
	var req llm.Request
	switch {
	case strings.TrimSpace(aiRequestFileFlag) != "":
		if err := readDocFile(aiRequestFileFlag, &req); err != nil {
			return llm.Request{}, validationErr("ai: parse request file: %v", err)
		}
	case strings.TrimSpace(aiRequestJSONFlag) != "":
		if err := json.Unmarshal([]byte(aiRequestJSONFlag), &req); err != nil {
			return llm.Request{}, validationErr("ai: parse request json: %v", err)
		}
	default:
		return llm.Request{}, validationErr("ai: request override not provided")
	}
	return req, nil
}

func applyAIRequestOverrides(req llm.Request) llm.Request {
	if aiProviderFlag != "" {
		req.ProviderHint = aiProviderFlag
	}
	if aiModelFlag != "" {
		req.ModelHint = aiModelFlag
	}
	if aiModeFlag != "" {
		req.Mode = aiModeFlag
	}
	if aiIntentFlag != "" {
		req.Intent = aiIntentFlag
	}
	if aiRequestIDFlag != "" {
		req.RequestID = aiRequestIDFlag
	}
	if aiSessionIDFlag != "" {
		req.SessionID = aiSessionIDFlag
	}
	if aiCallerIDFlag != "" {
		req.CallerID = aiCallerIDFlag
	}
	if aiMaxOutputFlag > 0 {
		req.MaxOutputTokens = aiMaxOutputFlag
	}
	if aiTokenBudgetFlag > 0 {
		req.TokenBudget = aiTokenBudgetFlag
	}
	if aiCostBudgetFlag > 0 {
		req.CostBudgetUSD = aiCostBudgetFlag
	}
	if aiLatencyTargetMS > 0 {
		req.LatencyTargetMS = aiLatencyTargetMS
	}
	if aiStreamFlag {
		req.Streaming = true
	}
	return req
}

func appendAIUserParts(req llm.Request, parts []llm.ContentPart) llm.Request {
	if len(parts) == 0 {
		return req
	}
	req.Input = append(req.Input, llm.Message{
		Role:  "user",
		Parts: append([]llm.ContentPart(nil), parts...),
	})
	return req
}

func buildAIUserParts(args []string) ([]llm.ContentPart, error) {
	text, err := readAITextArg(args)
	if err != nil {
		return nil, validationErr("ai: %v", err)
	}
	parts := make([]llm.ContentPart, 0, 1+len(aiImageFiles)+len(aiImageURLs))
	if strings.TrimSpace(text) != "" {
		parts = append(parts, llm.ContentPart{Type: "text", Text: text})
	}
	for _, path := range aiImageFiles {
		part, err := imagePartFromFile(path)
		if err != nil {
			return nil, validationErr("ai: %v", err)
		}
		parts = append(parts, part)
	}
	for _, rawURL := range aiImageURLs {
		part, err := imagePartFromURL(rawURL)
		if err != nil {
			return nil, validationErr("ai: %v", err)
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func readAITextArg(args []string) (string, error) {
	if len(args) == 0 {
		if len(aiImageFiles) > 0 || len(aiImageURLs) > 0 {
			return "", nil
		}
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if args[0] == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return args[0], nil
}

func imagePartFromFile(path string) (llm.ContentPart, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied file path is the purpose of the flag.
	if err != nil {
		return llm.ContentPart{}, fmt.Errorf("read image file %q: %w", path, err)
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return llm.ContentPart{}, fmt.Errorf("image file %q is not an image (detected %q)", path, mimeType)
	}
	return llm.ContentPart{
		Type:     "image",
		MIMEType: mimeType,
		Data:     data,
		Name:     filepath.Base(path),
	}, nil
}

func imagePartFromURL(rawURL string) (llm.ContentPart, error) {
	parsed, err := neturl.Parse(rawURL)
	if err != nil {
		return llm.ContentPart{}, fmt.Errorf("parse image url %q: %w", rawURL, err)
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(parsed.Path)))
	if !strings.HasPrefix(mimeType, "image/") {
		return llm.ContentPart{}, fmt.Errorf("image url %q requires an image-like extension so mime type can be inferred", rawURL)
	}
	return llm.ContentPart{
		Type:     "image",
		MIMEType: mimeType,
		URL:      rawURL,
		Name:     filepath.Base(parsed.Path),
	}, nil
}

func validateAISinceFlag() error {
	if aiSinceFlag == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, aiSinceFlag); err != nil {
		return validationErr("ai: --since must be RFC3339: %v", err)
	}
	return nil
}

func printAIChatResponse(resp llm.Response) {
	for _, msg := range resp.Output {
		for _, part := range msg.Parts {
			if part.Type == "text" && part.Text != "" {
				fmt.Println(part.Text)
			}
		}
	}
	printAIChatSummary(resp)
}

func printAIEmbeddingResponse(resp llm.Response) {
	fmt.Printf("provider: %s\nmodel: %s\nembeddings: %d\n", resp.Provider, resp.Model, len(resp.Embeddings))
	if resp.Usage.InputTokens > 0 {
		fmt.Printf("input tokens: %d\n", resp.Usage.InputTokens)
	}
	if len(resp.Embeddings) > 0 {
		fmt.Printf("dimensions: %d\n", len(resp.Embeddings[0].Vector))
	}
}

func printAIChatSummary(resp llm.Response) {
	if resp.Refusal != "" {
		fmt.Fprintf(os.Stderr, "refusal: %s\n", resp.Refusal)
	}
	fmt.Fprintf(os.Stderr, "provider=%s model=%s stop_reason=%s input_tokens=%d output_tokens=%d estimated_cost_usd=%.6f\n",
		resp.Provider,
		resp.Model,
		resp.StopReason,
		resp.Usage.InputTokens,
		resp.Usage.OutputTokens,
		resp.Usage.EstimatedCostUSD,
	)
}

func printAIUsage(out api.AIUsageResponse) {
	fmt.Printf("requests: %d\nsuccesses: %d\nerrors: %d\nlatency_ms: %d\ninput_tokens: %d\noutput_tokens: %d\ncache_read_tokens: %d\ncache_write_tokens: %d\nreasoning_tokens: %d\nestimated_cost_usd: %.6f\n",
		out.Requests,
		out.Successes,
		out.Errors,
		out.LatencyMs,
		out.InputTokens,
		out.OutputTokens,
		out.CacheReadTokens,
		out.CacheWriteTokens,
		out.ReasoningTokens,
		out.EstimatedCostUSD,
	)
	printAIUsageBreakdown("by_provider", out.ByProvider)
	printAIUsageBreakdown("by_model", out.ByModel)
	printAIUsageBreakdown("by_operation", out.ByOperation)
}

func printAIBudgets(out api.AIUsageBudgetsResponse) {
	if len(out.Budgets) == 0 {
		fmt.Println("count: 0")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tMODEL\tWINDOW START\tBUDGET\tSPENT USD\tREMAINING USD\tEXHAUSTED\tERROR")
	for _, budget := range out.Budgets {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%.6f\t%.6f\t%s\t%s\n",
			budget.Provider,
			budget.Model,
			budget.WindowStart,
			formatUsageBudget(&budget.UsageBudget),
			budget.SpentCostUSD,
			budget.RemainingCostUSD,
			strconv.FormatBool(budget.Exhausted),
			budget.Error,
		)
	}
	_ = w.Flush()
	fmt.Printf("\ncount: %d\n", out.Count)
}

func printAIRouteExplain(out api.RouteExplainResponse) {
	fmt.Printf("policy_version: %s\n", out.PolicyVersion)
	if out.Winner != nil {
		fmt.Printf("winner: %s/%s\n", out.Winner.Provider, out.Winner.Model)
		if out.Winner.EstimatedCostUSD > 0 {
			fmt.Printf("winner_estimated_cost_usd: %.6f\n", out.Winner.EstimatedCostUSD)
		}
	}
	if out.Error != "" {
		fmt.Printf("error: %s\n", out.Error)
	}
	if len(out.Candidates) == 0 {
		return
	}
	fmt.Println("\ncandidates:")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tMODEL\tMATCHED\tSELECTED\tALLOW REASONING\tALLOW TOOLS\tALLOW ATTACHMENTS\tMAX OUTPUT\tMAX COST USD\tUSAGE BUDGET\tERROR")
	for _, candidate := range out.Candidates {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			candidate.Provider,
			candidate.Model,
			strconv.FormatBool(candidate.Matched),
			strconv.FormatBool(candidate.Selected),
			formatOptionalBool(candidate.AllowReasoning),
			formatOptionalBool(candidate.AllowTools),
			formatOptionalBool(candidate.AllowAttachments),
			formatOptionalInt(candidate.MaxOutputTokens),
			formatOptionalFloat(candidate.MaxCostUSD),
			formatUsageBudget(candidate.UsageBudget),
			candidate.Error,
		)
	}
	_ = w.Flush()
	for _, candidate := range out.Candidates {
		if len(candidate.Reasons) == 0 {
			continue
		}
		fmt.Printf("\n%s/%s reasons:\n", candidate.Provider, candidate.Model)
		for _, reason := range candidate.Reasons {
			fmt.Printf("- %s\n", reason)
		}
	}
}

func printAIUsageBreakdown(title string, rows []api.AIUsageBreakdownDTO) {
	if len(rows) == 0 {
		return
	}
	fmt.Printf("\n%s:\n", title)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tREQUESTS\tOK\tERRORS\tLATENCY\tINPUT TOKENS\tOUTPUT TOKENS\tCOST USD")
	for _, row := range rows {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%.6f\n",
			row.Key, row.Requests, row.Successes, row.Errors, row.LatencyMs, row.InputTokens, row.OutputTokens, row.EstimatedCostUSD)
	}
	_ = w.Flush()
}

func printAIAudit(out api.AIAuditListResponse) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTIMESTAMP\tEVENT\tPROVIDER\tMODEL\tOK\tLATENCY\tINPUT TOKENS\tOUTPUT TOKENS\tERROR")
	for _, ev := range out.Events {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			ev.ID,
			ev.Timestamp,
			ev.EventType,
			ev.Provider,
			ev.Model,
			strconv.FormatBool(ev.Success),
			ev.LatencyMs,
			ev.InputTokens,
			ev.OutputTokens,
			firstNonEmptyString(ev.Error, ev.Refusal),
		)
	}
	_ = w.Flush()
	fmt.Printf("\ncount: %d\n", out.Count)
}

type aiBudgetRejectionPayload struct {
	RequestID     string `json:"request_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	CallerID      string `json:"caller_id,omitempty"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	PolicyVersion string `json:"policy_version,omitempty"`
	Error         string `json:"error"`
}

func parseAIBudgetRejectionPayload(raw string) (aiBudgetRejectionPayload, error) {
	var out aiBudgetRejectionPayload
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return aiBudgetRejectionPayload{}, fmt.Errorf("ai: decode budget rejection payload: %w", err)
	}
	return out, nil
}

func matchesAIBudgetWatchFilters(payload aiBudgetRejectionPayload) bool {
	if aiProviderFlag != "" && payload.Provider != aiProviderFlag {
		return false
	}
	if aiModelFlag != "" && payload.Model != aiModelFlag {
		return false
	}
	if aiCallerIDFlag != "" && payload.CallerID != aiCallerIDFlag {
		return false
	}
	if aiSessionIDFlag != "" && payload.SessionID != aiSessionIDFlag {
		return false
	}
	return true
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func formatOptionalBool(v *bool) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatBool(*v)
}

func formatOptionalInt(v *int) string {
	if v == nil {
		return "-"
	}
	return strconv.Itoa(*v)
}

func formatOptionalFloat(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.6f", *v)
}

func formatUsageBudget(v *api.AIUsageBudgetPolicyDTO) string {
	if v == nil || v.MaxCostUSD == nil {
		return "-"
	}
	window := v.Window
	if window == "" {
		window = "month"
	}
	scope := v.Scope
	if scope == "" {
		scope = "total"
	}
	level := v.Level
	if level == "" {
		level = "route"
	}
	return fmt.Sprintf("%s/%s/%.6f@%s", scope, window, *v.MaxCostUSD, level)
}

func resetAIFlags() {
	aiJSONFlag = false
	aiProviderFlag = ""
	aiModelFlag = ""
	aiRequestFileFlag = ""
	aiRequestJSONFlag = ""
	aiSystemFlag = ""
	aiStreamFlag = false
	aiImageFiles = nil
	aiImageURLs = nil
	aiSessionIDFlag = ""
	aiCallerIDFlag = ""
	aiRequestIDFlag = ""
	aiModeFlag = ""
	aiIntentFlag = ""
	aiMaxOutputFlag = 0
	aiTokenBudgetFlag = 0
	aiCostBudgetFlag = 0
	aiLatencyTargetMS = 0
	aiAuditEventTypeFlag = ""
	aiAuditErrorsOnly = false
	aiAuditLimit = 0
	aiSinceFlag = ""
}

func init() {
	aiCmd.PersistentFlags().BoolVar(&aiJSONFlag, "json", false, "emit raw JSON instead of the pretty rendering")
	aiCmd.PersistentFlags().StringVar(&aiProviderFlag, "provider", "", "configured provider id filter or hint")
	aiCmd.PersistentFlags().StringVar(&aiModelFlag, "model", "", "model filter or routing hint")
	aiCmd.PersistentFlags().StringVar(&aiSessionIDFlag, "session-id", "", "session correlation id")
	aiCmd.PersistentFlags().StringVar(&aiCallerIDFlag, "caller-id", "", "caller correlation id")
	aiCmd.PersistentFlags().StringVar(&aiSinceFlag, "since", "", "RFC3339 lower-bound timestamp filter")

	aiChatCmd.Flags().StringVar(&aiSystemFlag, "system", "", "optional system prompt")
	aiChatCmd.Flags().BoolVar(&aiStreamFlag, "stream", false, "stream incremental response events over SSE")
	aiChatCmd.Flags().StringArrayVar(&aiImageFiles, "image-file", nil, "append an image file as a user content part")
	aiChatCmd.Flags().StringArrayVar(&aiImageURLs, "image-url", nil, "append an image URL as a user content part")
	aiChatCmd.Flags().StringVar(&aiRequestFileFlag, "request-file", "", "path to a normalized AI request in JSON or YAML")
	aiChatCmd.Flags().StringVar(&aiRequestJSONFlag, "request-json", "", "inline normalized AI request JSON")
	aiChatCmd.Flags().StringVar(&aiRequestIDFlag, "request-id", "", "request correlation id")
	aiChatCmd.Flags().StringVar(&aiModeFlag, "mode", "", "mode hint")
	aiChatCmd.Flags().StringVar(&aiIntentFlag, "intent", "", "intent hint")
	aiChatCmd.Flags().IntVar(&aiMaxOutputFlag, "max-output-tokens", 0, "max output token hint")
	aiChatCmd.Flags().IntVar(&aiTokenBudgetFlag, "token-budget", 0, "token budget hint")
	aiChatCmd.Flags().Float64Var(&aiCostBudgetFlag, "cost-budget-usd", 0, "cost budget hint in USD")
	aiChatCmd.Flags().IntVar(&aiLatencyTargetMS, "latency-target-ms", 0, "latency target hint in milliseconds")

	aiEmbeddingsCmd.Flags().StringVar(&aiRequestFileFlag, "request-file", "", "path to a normalized AI request in JSON or YAML")
	aiEmbeddingsCmd.Flags().StringVar(&aiRequestJSONFlag, "request-json", "", "inline normalized AI request JSON")
	aiEmbeddingsCmd.Flags().StringVar(&aiRequestIDFlag, "request-id", "", "request correlation id")
	aiEmbeddingsCmd.Flags().StringVar(&aiModeFlag, "mode", "", "mode hint")
	aiEmbeddingsCmd.Flags().StringVar(&aiIntentFlag, "intent", "", "intent hint")
	aiEmbeddingsCmd.Flags().IntVar(&aiTokenBudgetFlag, "token-budget", 0, "token budget hint")
	aiEmbeddingsCmd.Flags().Float64Var(&aiCostBudgetFlag, "cost-budget-usd", 0, "cost budget hint in USD")
	aiEmbeddingsCmd.Flags().IntVar(&aiLatencyTargetMS, "latency-target-ms", 0, "latency target hint in milliseconds")

	aiRoutePreviewCmd.Flags().StringVar(&aiSystemFlag, "system", "", "optional system prompt")
	aiRoutePreviewCmd.Flags().StringArrayVar(&aiImageFiles, "image-file", nil, "append an image file as a user content part")
	aiRoutePreviewCmd.Flags().StringArrayVar(&aiImageURLs, "image-url", nil, "append an image URL as a user content part")
	aiRoutePreviewCmd.Flags().StringVar(&aiRequestFileFlag, "request-file", "", "path to a normalized AI request in JSON or YAML")
	aiRoutePreviewCmd.Flags().StringVar(&aiRequestJSONFlag, "request-json", "", "inline normalized AI request JSON")
	aiRoutePreviewCmd.Flags().StringVar(&aiRequestIDFlag, "request-id", "", "request correlation id")
	aiRoutePreviewCmd.Flags().StringVar(&aiModeFlag, "mode", "", "mode hint")
	aiRoutePreviewCmd.Flags().StringVar(&aiIntentFlag, "intent", "", "intent hint")
	aiRoutePreviewCmd.Flags().IntVar(&aiMaxOutputFlag, "max-output-tokens", 0, "max output token hint")
	aiRoutePreviewCmd.Flags().IntVar(&aiTokenBudgetFlag, "token-budget", 0, "token budget hint")
	aiRoutePreviewCmd.Flags().Float64Var(&aiCostBudgetFlag, "cost-budget-usd", 0, "cost budget hint in USD")
	aiRoutePreviewCmd.Flags().IntVar(&aiLatencyTargetMS, "latency-target-ms", 0, "latency target hint in milliseconds")

	aiRouteExplainCmd.Flags().StringVar(&aiSystemFlag, "system", "", "optional system prompt")
	aiRouteExplainCmd.Flags().StringArrayVar(&aiImageFiles, "image-file", nil, "append an image file as a user content part")
	aiRouteExplainCmd.Flags().StringArrayVar(&aiImageURLs, "image-url", nil, "append an image URL as a user content part")
	aiRouteExplainCmd.Flags().StringVar(&aiRequestFileFlag, "request-file", "", "path to a normalized AI request in JSON or YAML")
	aiRouteExplainCmd.Flags().StringVar(&aiRequestJSONFlag, "request-json", "", "inline normalized AI request JSON")
	aiRouteExplainCmd.Flags().StringVar(&aiRequestIDFlag, "request-id", "", "request correlation id")
	aiRouteExplainCmd.Flags().StringVar(&aiModeFlag, "mode", "", "mode hint")
	aiRouteExplainCmd.Flags().StringVar(&aiIntentFlag, "intent", "", "intent hint")
	aiRouteExplainCmd.Flags().IntVar(&aiMaxOutputFlag, "max-output-tokens", 0, "max output token hint")
	aiRouteExplainCmd.Flags().IntVar(&aiTokenBudgetFlag, "token-budget", 0, "token budget hint")
	aiRouteExplainCmd.Flags().Float64Var(&aiCostBudgetFlag, "cost-budget-usd", 0, "cost budget hint in USD")
	aiRouteExplainCmd.Flags().IntVar(&aiLatencyTargetMS, "latency-target-ms", 0, "latency target hint in milliseconds")

	aiAuditCmd.Flags().StringVar(&aiAuditEventTypeFlag, "event-type", "", "event type filter")
	aiAuditCmd.Flags().BoolVar(&aiAuditErrorsOnly, "errors-only", false, "show only failed audit events")
	aiAuditCmd.Flags().IntVar(&aiAuditLimit, "limit", 0, "max rows to return")

	aiCmd.AddCommand(aiProvidersCmd, aiModelsCmd, aiRoutesCmd, aiChatCmd, aiEmbeddingsCmd, aiRoutePreviewCmd, aiRouteExplainCmd, aiUsageCmd, aiBudgetsCmd, aiAuditCmd, aiWatchBudgetsCmd)
}
