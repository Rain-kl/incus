package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// PayIncus: Xray maintenance runs without a per-action confirmation and returns raw metadata for a friendly report.
func (b *backend) xray(ctx context.Context, action string) ([]byte, error) {
	if action != "inspect" && action != "test" && action != "restart" && action != "reconfigure" {
		return nil, errors.New("unknown Xray action")
	}
	client := newLocalClient()
	defer client.http.CloseIdleConnections()
	method := "POST"
	var input any = map[string]string{"action": action}
	if action == "inspect" {
		method, input = "GET", nil
	}
	return client.call(ctx, method, "/1.0/payincus/xray", input)
}

func xrayOperation(ctx context.Context, cancel context.CancelFunc, b *backend, action string) tea.Cmd {
	return func() tea.Msg {
		defer cancel()
		logOperation(ctx, "操作开始")
		data, err := b.xray(ctx, action)
		if err != nil {
			logOperation(ctx, "操作失败：%v", err)
			return resultMsg{err: err}
		}
		report, err := xrayReport(action, data)
		if err != nil {
			logOperation(ctx, "操作失败：%v", err)
			return resultMsg{err: err}
		}
		logOperation(ctx, "操作完成")
		return resultMsg{report: report}
	}
}

type xrayStatus struct {
	State           string   `json:"state"`
	AppliedRevision int      `json:"appliedRevision"`
	Error           string   `json:"error"`
	ErrorStage      string   `json:"errorStage"`
	ErrorDetail     string   `json:"errorDetail"`
	XrayAvailable   bool     `json:"xrayAvailable"`
	ActiveNodeID    int      `json:"activeNodeId"`
	HealthyNodeIDs  []int    `json:"healthyNodeIds"`
	ManagedNetworks []string `json:"managedNetworks"`
	RoutedFallback  bool     `json:"routedFallback"`
}

type xrayNode struct {
	ID       int    `json:"id"`
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
}

type xrayProbe struct {
	NodeID    int    `json:"nodeId"`
	Success   bool   `json:"success"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error"`
}

// PayIncus: render the local Xray API metadata as readable lines instead of a raw JSON/log dump.
func xrayReport(action string, data []byte) ([]string, error) {
	switch action {
	case "inspect":
		var payload struct {
			Status xrayStatus `json:"status"`
			Nodes  []xrayNode `json:"nodes"`
		}
		err := json.Unmarshal(data, &payload)
		if err != nil {
			return nil, fmt.Errorf("解析 Xray 状态：%w", err)
		}
		lines := []string{"Xray 节点与运行状态", ""}
		lines = append(lines, xrayStatusLines(payload.Status)...)
		lines = append(lines, "", fmt.Sprintf("代理节点（%d）", len(payload.Nodes)))
		if len(payload.Nodes) == 0 {
			lines = append(lines, "  未配置代理节点")
			return lines, nil
		}
		for _, node := range payload.Nodes {
			lines = append(lines, fmt.Sprintf("  #%d  %s  %s", node.ID, xrayProtocol(node.Protocol), xrayEndpoint(node.Address, node.Port)))
		}
		return lines, nil
	case "test":
		var payload struct {
			Items []xrayProbe `json:"items"`
		}
		err := json.Unmarshal(data, &payload)
		if err != nil {
			return nil, fmt.Errorf("解析 Xray 测试结果：%w", err)
		}
		sort.Slice(payload.Items, func(i, j int) bool { return payload.Items[i].NodeID < payload.Items[j].NodeID })
		successes := 0
		for _, item := range payload.Items {
			if item.Success {
				successes++
			}
		}
		lines := []string{"Xray 代理节点测试", "", fmt.Sprintf("共 %d 个节点，成功 %d，失败 %d", len(payload.Items), successes, len(payload.Items)-successes), ""}
		if len(payload.Items) == 0 {
			lines = append(lines, "  未配置代理节点")
			return lines, nil
		}
		for _, item := range payload.Items {
			lines = append(lines, xrayProbeLine(item))
		}
		return lines, nil
	case "restart", "reconfigure":
		var status xrayStatus
		err := json.Unmarshal(data, &status)
		if err != nil {
			return nil, fmt.Errorf("解析 Xray 运行状态：%w", err)
		}
		title := "Xray 服务已重启"
		if action == "reconfigure" {
			title = "Xray 运行环境已重建"
		}
		lines := []string{title, ""}
		lines = append(lines, xrayStatusLines(status)...)
		if status.AppliedRevision == 0 {
			lines = append(lines, "", "本节点尚未下发代理策略：已重建运行环境，暂不接管流量。")
		}
		return lines, nil
	}
	return nil, errors.New("unknown Xray action")
}

func xrayStatusLines(status xrayStatus) []string {
	core := "不可用"
	if status.XrayAvailable {
		core = "可用"
	}
	lines := []string{
		"状态：" + xrayStateName(status.State) + "    策略版本：" + strconv.Itoa(status.AppliedRevision),
		"Xray 核心：" + core,
	}
	if status.ActiveNodeID > 0 {
		lines = append(lines, "活动节点：#"+strconv.Itoa(status.ActiveNodeID))
	}
	lines = append(lines, "健康节点："+xrayNodeIDs(status.HealthyNodeIDs))
	if len(status.ManagedNetworks) > 0 {
		lines = append(lines, "托管网络："+strings.Join(status.ManagedNetworks, "、"))
	}
	fallback := "否"
	if status.RoutedFallback {
		fallback = "是"
	}
	lines = append(lines, "回退路由："+fallback)
	if status.Error != "" && status.Error != "none" {
		detail := status.Error
		if status.ErrorStage != "" {
			detail += "（阶段：" + status.ErrorStage + "）"
		}
		if status.ErrorDetail != "" {
			detail += " " + status.ErrorDetail
		}
		lines = append(lines, "错误："+detail)
	}
	return lines
}

func xrayStateName(state string) string {
	switch state {
	case "applied":
		return "已应用"
	case "applying":
		return "应用中"
	case "idle":
		return "空闲"
	case "error":
		return "异常"
	case "":
		return "未知"
	}
	return state
}

func xrayNodeIDs(ids []int) string {
	if len(ids) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, "#"+strconv.Itoa(id))
	}
	return strings.Join(parts, " ")
}

func xrayProtocol(protocol string) string {
	if strings.TrimSpace(protocol) == "" {
		return "未知协议"
	}
	return protocol
}

func xrayEndpoint(address string, port int) string {
	if strings.TrimSpace(address) == "" {
		return "未提供地址"
	}
	return address + ":" + strconv.Itoa(port)
}

func xrayProbeLine(item xrayProbe) string {
	if item.Success {
		return fmt.Sprintf("  #%d  成功  %d ms", item.NodeID, item.LatencyMs)
	}
	detail := strings.TrimSpace(item.Error)
	if detail == "" {
		detail = "未知错误"
	}
	return fmt.Sprintf("  #%d  失败  %s", item.NodeID, detail)
}
