package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/itchyny/gojq"
)

type Zoro struct{}

type Spec struct {
	jqQueries map[string]*gojq.Query
	json      specJSON
}

type specStepJSON struct {
	Split     string `json:"split,omitempty"`
	JQ        string `json:"jq,omitempty"`
	RawOutput bool   `json:"rawOutput"`
}

type specJSON struct {
	URL string `json:"url"`

	Request  []specStepJSON `json:"request"`
	Response []specStepJSON `json:"response"`
}

func (z *Zoro) Spec(ctx context.Context, specURL string) (*Spec, error) {
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

	ret := Spec{
		json:      sj,
		jqQueries: map[string]*gojq.Query{},
	}

	if err := ret.prepareSteps(sj.Request); err != nil {
		return nil, fmt.Errorf("request steps: %w", err)
	}

	if err := ret.prepareSteps(sj.Response); err != nil {
		return nil, fmt.Errorf("response steps: %w", err)
	}

	return &ret, nil
}

func (z *Zoro) SpecExec(
	ctx context.Context,
	specURL string,
	vars map[string]string,
) ([]byte, error) {
	spec, err := z.Spec(ctx, specURL)
	if err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}

	bts, err := spec.Exec(ctx, vars)
	if err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}

	return bts, nil
}

func (s *Spec) Exec(ctx context.Context, vars map[string]string) ([]byte, error) {
	payload, err := s.doSteps(ctx, nil, s.json.Request, vars)
	if err != nil {
		return nil, fmt.Errorf("request steps: %w", err)
	}

	fmt.Println("DEBUG payload", string(payload))

	input, err := s.fetchInput(ctx, payload)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}

	fmt.Println("DEBUG", string(input))

	output, err := s.doSteps(ctx, input, s.json.Response, vars)
	if err != nil {
		return nil, fmt.Errorf("response steps: %w", err)
	}

	return output, nil
}

func (s *Spec) fetchInput(ctx context.Context, payload []byte) ([]byte, error) {
	method := "GET"
	if len(payload) > 0 {
		method = "POST"
	}

	fmt.Printf("DEBUG Payload: %s\n", string(payload))

	req, err := http.NewRequestWithContext(ctx, method, s.json.URL, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Add("content-type", "application/x-www-form-urlencoded")

	fmt.Println("DEBUG req", req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func (s *Spec) prepareSteps(steps []specStepJSON) error {
	for _, step := range steps {
		if step.JQ == "" {
			continue
		}

		query, err := gojq.Parse(step.JQ)
		if err != nil {
			return err
		}

		s.jqQueries[step.JQ] = query
	}

	return nil
}

func (s *Spec) doSteps(ctx context.Context, input []byte, steps []specStepJSON, vars map[string]string) ([]byte, error) {
	var err error

	for _, step := range steps {
		if step.Split != "" {
			input, err = json.Marshal(strings.Split(string(input), step.Split))
			if err != nil {
				return nil, fmt.Errorf("split: %w", err)
			}
		}

		if step.JQ != "" {
			input, err = s.stepJQ(ctx, input, step.JQ, vars, step.RawOutput)
			if err != nil {
				return nil, fmt.Errorf("jq: %w", err)
			}
		}
	}

	return input, nil
}

func (s *Spec) stepJQ(ctx context.Context, input []byte, query string, vars map[string]string, rawOutput bool) ([]byte, error) {
	keys := make([]string, 0, len(vars))
	vals := make([]interface{}, 0, len(vars))

	for k, v := range vars {
		keys = append(keys, "$"+k)
		vals = append(vals, v)
	}

	code, err := gojq.Compile(s.jqQueries[query], gojq.WithVariables(keys))
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}

	fmt.Println("DEBUG stepJQ input", string(input))

	var inputIface interface{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &inputIface); err != nil {
			return nil, fmt.Errorf("unmarshal: %w", err)
		}
	}

	iter := code.RunWithContext(ctx, inputIface, vals...)
	v, _ := iter.Next()
	if err, ok := v.(error); ok {
		return nil, fmt.Errorf("run: %w", err)
	}

	var bts []byte

	switch typedV := v.(type) {
	case string:
		bts = []byte(typedV)
	case []byte:
		bts = typedV
	default:
	}

	fmt.Println(reflect.TypeOf(v))

	if len(bts) > 0 && rawOutput {
		fmt.Println("DEBUG stepJQ raw output", string(bts))
		return bts, nil
	}

	bts, err = json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	fmt.Println("DEBUG stepJQ output", string(bts))

	return bts, nil
}
