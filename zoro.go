package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/itchyny/gojq"
)

type Zoro struct{}

type Spec struct {
	jqQueries map[string]*gojq.Query
	json      specJSON
}

type specReturnIf struct {
	Return any    `json:"return"`
	If     string `json:"if"`
}

type specStep struct {
	Split    string        `json:"split,omitempty"`
	JQ       string        `json:"jq,omitempty"`
	ReturnIf *specReturnIf `json:"returnIf,omitempty"`
}

type specItem struct {
	URL string `json:"url"`

	Request  []specStep `json:"request"`
	Response []specStep `json:"response"`
	Retry    []specStep `json:"retry"`
}

type specJSON []specItem

func (z *Zoro) fetchSpec(ctx context.Context, specURL string) (specJSON, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", specURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	sj := specJSON{}
	dec := json.NewDecoder(resp.Body)

	if err := dec.Decode(&sj); err != nil {
		return nil, err
	}

	return sj, nil
}

func (z *Zoro) Spec(ctx context.Context, specURL string) (*Spec, error) {
	sj, err := z.fetchSpec(ctx, specURL)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}

	ret := Spec{
		json:      sj,
		jqQueries: map[string]*gojq.Query{},
	}

	for i, item := range sj {
		if err := ret.prepareSteps(item.Request); err != nil {
			return nil, fmt.Errorf("item %d: request: %w", i, err)
		}

		if err := ret.prepareSteps(item.Response); err != nil {
			return nil, fmt.Errorf("item %d: response: %w", i, err)
		}

		if err := ret.prepareSteps(item.Retry); err != nil {
			return nil, fmt.Errorf("item %d: retry: %w", i, err)
		}
	}

	return &ret, nil
}

func (z *Zoro) SpecExecHTTP(
	ctx context.Context,
	specURL string,
	httpReqParams HttpRequestParams,
) ([]byte, error) {
	spec, err := z.Spec(ctx, specURL)
	if err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}

	payload, err := json.Marshal(httpReqParams)
	if err != nil {
		return nil, fmt.Errorf("marshal http params: %w", httpReqParams)
	}

	bts, err := spec.Exec(ctx, payload)
	if err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}

	return bts, nil
}

type stepsResult struct {
	payload   []byte
	returnNow bool
}

func (s *Spec) execSteps(ctx context.Context, steps []specStep, payload []byte) (*stepsResult, error) {
	var err error

	for _, step := range steps {
		if step.Split != "" {
			payload, err = json.Marshal(strings.Split(string(payload), step.Split))
			if err != nil {
				return nil, fmt.Errorf("split: %w", err)
			}
		}

		if step.JQ != "" {
			payload, err = s.execStepJQ(ctx, s.jqQueries[step.JQ], payload)
			if err != nil {
				return nil, fmt.Errorf("jq: %w", err)
			}
		}

		if step.ReturnIf != nil {
			ifBts, err := s.execStepJQ(ctx, s.jqQueries[step.ReturnIf.If], payload)
			if err != nil {
				return nil, fmt.Errorf("returnIf: if: exec: %w", err)
			}

			ifBool, err := strconv.ParseBool(string(ifBts))
			if err != nil {
				return nil, fmt.Errorf("returnIf: if: parse: %w", err)
			}

			if ifBool {
				payload, err = json.Marshal(step.ReturnIf.Return)
				if err != nil {
					return nil, fmt.Errorf("returnIf: return: marshal: %w", err)
				}

				return &stepsResult{
					payload:   payload,
					returnNow: true,
				}, nil
			}
		}
	}

	return &stepsResult{
		payload: payload,
	}, nil
}

type itemResult stepsResult

func (s *Spec) execItem(ctx context.Context, item specItem, input []byte) (*itemResult, error) {
retry:
	fmt.Println("DEBUG item input", string(input))

	request, err := s.execSteps(ctx, item.Request, input)
	if err != nil {
		return nil, fmt.Errorf("request steps: %w", err)
	}

	if request.returnNow {
		return (*itemResult)(request), nil
	}

	fmt.Println("DEBUG payload", string(request.payload))

	var reqParams HttpRequestParams

	if err := json.Unmarshal(request.payload, &reqParams); err != nil {
		return nil, fmt.Errorf("request params: %w", err)
	}

	response, err := s.httpRequest(ctx, item.URL, reqParams)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}

	fmt.Println("DEBUG", string(response))

	output, err := s.execSteps(ctx, item.Response, response)
	if err != nil {
		return nil, fmt.Errorf("response steps: %w", err)
	}

	if output.returnNow {
		return (*itemResult)(output), nil
	}

	if len(item.Retry) > 0 {
		retryResult, err := s.execSteps(ctx, item.Retry, output.payload)
		if err != nil {
			return nil, fmt.Errorf("retry: exec: %w", err)
		}

		if retryResult.returnNow {
			return (*itemResult)(retryResult), nil
		}

		fmt.Println("DEBUG retry bytes", string(retryResult.payload))

		retryBool, err := strconv.ParseBool(string(retryResult.payload))
		if err != nil {
			return nil, fmt.Errorf("retry: output: %w", err)
		}

		if retryBool {
			fmt.Println("DEBUG retrying")
			goto retry
		}
	}

	return (*itemResult)(output), nil
}

func (s *Spec) Exec(ctx context.Context, input []byte) ([]byte, error) {
	payload := input

	for i, item := range s.json {
		res, err := s.execItem(ctx, item, payload)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}

		payload = res.payload

		if res.returnNow {
			return payload, nil
		}
	}

	return payload, nil
}

type HttpRequestParams struct {
	Path    map[string]string `json:"path"`
	Query   map[string]string `json:"query"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
	Method  string            `json:"method"`
}

func (s *Spec) httpRequest(ctx context.Context, rawURL string, params HttpRequestParams) ([]byte, error) {
	method := params.Method
	var body io.Reader

	if len(params.Body) > 0 {
		body = bytes.NewBuffer([]byte(params.Body))
		if params.Method == "" {
			method = "POST"
		}
	}

	if method == "" {
		method = "GET"
	}

	fmt.Printf("DEBUG Body: %s\n", string(params.Body))

	for k, v := range params.Path {
		rawURL = strings.ReplaceAll(rawURL, "$"+k, v)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}

	queryVals := u.Query()
	for k, v := range params.Query {
		queryVals.Add(k, v)
	}

	u.RawQuery = queryVals.Encode()

	fmt.Printf("DEBUG url: %s\n", u.String())

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}

	for k, v := range params.Headers {
		req.Header.Add(k, v)
	}

	fmt.Println("DEBUG req", req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func (s *Spec) parseJQ(jq string) error {
	query, err := gojq.Parse(jq)
	if err != nil {
		return err
	}

	s.jqQueries[jq] = query

	return nil
}

func (s *Spec) prepareSteps(steps []specStep) error {
	for i, step := range steps {
		if step.JQ != "" {
			if err := s.parseJQ(step.JQ); err != nil {
				return fmt.Errorf("step %d: parse jq: %w", i, err)
			}
		}

		if step.ReturnIf != nil {
			if err := s.parseJQ(step.ReturnIf.If); err != nil {
				return fmt.Errorf("step %d: parse returnIf: %w", i, err)
			}
		}
	}

	return nil
}

func (s *Spec) execStepJQ(ctx context.Context, query *gojq.Query, input []byte) ([]byte, error) {
	var inputIface interface{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &inputIface); err != nil {
			return nil, fmt.Errorf("unmarshal: %w", err)
		}
	}

	iter := query.RunWithContext(ctx, inputIface)
	v, _ := iter.Next()
	if err, ok := v.(error); ok {
		return nil, fmt.Errorf("run: %w", err)
	}

	bts, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	fmt.Println("DEBUG stepJQ output", string(bts))

	return bts, nil
}
