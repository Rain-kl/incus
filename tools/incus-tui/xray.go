package main

import (
	"context"
	"encoding/json"
	"errors"
)

func (b *backend) xray(ctx context.Context, action string) error {
	if action != "inspect" && action != "test" && action != "restart" && action != "reconfigure" {
		return errors.New("unknown Xray action")
	}
	client := newLocalClient()
	defer client.http.CloseIdleConnections()
	method := "POST"
	var input any = map[string]string{"action": action}
	if action == "inspect" {
		method, input = "GET", nil
	}
	data, err := client.call(ctx, method, "/1.0/payincus/xray", input)
	if err != nil {
		return err
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return errors.New("invalid Xray response")
	}
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	logOperation(ctx, "Xray %s:\n%s", action, pretty)
	return nil
}
