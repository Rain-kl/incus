package tunnel

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/lxc/incus/v7/internal/server/payincus/nodeclient"
)

type taskLogControl struct {
	TaskID int    `json:"taskId"`
	Name   string `json:"name"`
	Action string `json:"action"`
}

var taskInstanceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,99}$`)

func streamTaskLogs(ctx context.Context, out chan<- []byte, ctl taskLogControl, client *nodeclient.Client) {
	if client == nil {
		return
	}

	emit := func(line string) {
		payload, _ := json.Marshal(map[string]any{"taskId": ctl.TaskID, "line": line})
		select {
		case out <- EncodeFrame(FrameTypeTaskLogData, 0, payload):
		case <-ctx.Done():
		}
	}

	err := client.Stream(ctx, ctl.Name, emit)
	if err != nil && ctx.Err() == nil {
		emit("节点日志 API 连接失败，请检查 incusd 状态与接口权限")
	}
}
