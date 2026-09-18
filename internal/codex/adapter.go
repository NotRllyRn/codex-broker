package codex

import (
	"context"
	"errors"
	"sort"
)

type LoginInteraction struct {
	LoginID, Method, AuthURL, VerificationURL, UserCode string
	ExpiresAtMS                                         int64
}

type Adapter struct{ Client *Client }

func (a Adapter) StartLogin(ctx context.Context, method string) (LoginInteraction, error) {
	if method == "MANUAL_TOKENS" {
		return LoginInteraction{}, errors.New("manual token import does not start OAuth")
	}
	kind := "chatgptDeviceCode"
	params := map[string]any{"type": kind}
	if method == "CHATGPT_BROWSER" {
		params = map[string]any{"type": "chatgpt", "useHostedLoginSuccessPage": true, "appBrand": "codex"}
	}
	result, err := a.Client.Request(ctx, "account/login/start", params)
	if err != nil {
		return LoginInteraction{}, err
	}
	return LoginInteraction{LoginID: text(result["loginId"]), Method: method, AuthURL: text(result["authUrl"]), VerificationURL: text(result["verificationUrl"]), UserCode: text(result["userCode"]), ExpiresAtMS: integer(result["expiresAt"])}, nil
}

func (a Adapter) CancelLogin(ctx context.Context, loginID string) error {
	_, err := a.Client.Request(ctx, "account/login/cancel", map[string]any{"loginId": loginID})
	return err
}

func (a Adapter) Account(ctx context.Context, refresh bool) (map[string]any, error) {
	return a.Client.Request(ctx, "account/read", map[string]any{"refreshToken": refresh})
}

func (a Adapter) RateLimits(ctx context.Context) (map[string]any, error) {
	return a.Client.Request(ctx, "account/rateLimits/read", map[string]any{})
}

func (a Adapter) PulseWindows(ctx context.Context) (map[string]any, error) {
	listed, err := a.Client.Request(ctx, "model/list", map[string]any{"limit": 100})
	if err != nil {
		return nil, err
	}
	model, effort, err := pulseModel(listed["data"])
	if err != nil {
		return nil, err
	}
	started, err := a.Client.Request(ctx, "thread/start", map[string]any{"ephemeral": true, "model": model, "serviceTier": "default"})
	if err != nil {
		return nil, err
	}
	threadID := nestedText(started, "thread", "id")
	if threadID == "" {
		return nil, errors.New("Codex returned no window-pulse thread ID")
	}
	turn, err := a.Client.Request(ctx, "turn/start", map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "Reply OK."}}, "model": model, "effort": effort, "serviceTier": "default"})
	if err != nil {
		return nil, err
	}
	turnID := nestedText(turn, "turn", "id")
	if turnID == "" {
		return nil, errors.New("Codex returned no window-pulse turn ID")
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-a.Client.Done():
			return nil, a.Client.Err()
		case message := <-a.Client.Notifications():
			if text(message["method"]) != "turn/completed" {
				continue
			}
			params, _ := message["params"].(map[string]any)
			completed, _ := params["turn"].(map[string]any)
			if text(completed["id"]) != turnID {
				continue
			}
			if text(completed["status"]) != "completed" {
				return nil, errors.New("Codex window pulse failed")
			}
			return a.RateLimits(ctx)
		}
	}
}

func pulseModel(raw any) (string, string, error) {
	type candidate struct {
		rank          int
		model, effort string
	}
	var candidates []candidate
	items, _ := raw.([]any)
	for _, rawItem := range items {
		item, _ := rawItem.(map[string]any)
		if item == nil || item["hidden"] == true || text(item["model"]) == "" || !listHas(item["inputModalities"], "text") {
			continue
		}
		var efforts []string
		for _, rawEffort := range list(item["supportedReasoningEfforts"]) {
			if value, ok := rawEffort.(map[string]any); ok && text(value["reasoningEffort"]) != "" {
				efforts = append(efforts, text(value["reasoningEffort"]))
			}
		}
		if len(efforts) == 0 {
			continue
		}
		effort := efforts[0]
		if has(efforts, "minimal") {
			effort = "minimal"
		} else if has(efforts, "low") {
			effort = "low"
		}
		rank := 1
		if contains(text(item["model"]), "mini") {
			rank = 0
		}
		candidates = append(candidates, candidate{rank, text(item["model"]), effort})
	}
	if len(candidates) == 0 {
		return "", "", errors.New("Codex returned no text model for window pulse")
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].rank != candidates[j].rank {
			return candidates[i].rank < candidates[j].rank
		}
		return candidates[i].model < candidates[j].model
	})
	return candidates[0].model, candidates[0].effort, nil
}

func text(value any) string {
	result, _ := value.(string)
	return result
}
func integer(value any) int64 {
	if v, ok := value.(float64); ok {
		return int64(v)
	}
	return 0
}
func list(value any) []any {
	result, _ := value.([]any)
	return result
}
func listHas(value any, wanted string) bool {
	for _, item := range list(value) {
		if text(item) == wanted {
			return true
		}
	}
	return false
}
func has(values []string, wanted string) bool {
	for _, item := range values {
		if item == wanted {
			return true
		}
	}
	return false
}
func nestedText(value map[string]any, outer, inner string) string {
	nested, _ := value[outer].(map[string]any)
	if nested == nil {
		return ""
	}
	return text(nested[inner])
}
