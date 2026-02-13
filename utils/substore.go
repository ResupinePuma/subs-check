package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/beck-8/subs-check/config"
)

type sub struct {
	Content string           `json:"content"`
	Name    string           `json:"name"`
	Remark  string           `json:"remark"`
	Source  string           `json:"source"`
	Process []map[string]any `json:"process"`
}

type subResult struct {
	Data   sub    `json:"data"`
	Status string `json:"status"`
}

type args struct {
	Content string `json:"content"`
	Mode    string `json:"mode"`
}

type Operator struct {
	Args     args   `json:"args"`
	Disabled bool   `json:"disabled"`
	Type     string `json:"type"`
}

type file struct {
	Name       string     `json:"name"`
	Process    []Operator `json:"process"`
	Remark     string     `json:"remark"`
	Source     string     `json:"source"`
	SourceName string     `json:"sourceName"`
	SourceType string     `json:"sourceType"`
	Type       string     `json:"type"`
}

type fileResult struct {
	Data   file   `json:"data"`
	Status string `json:"status"`
}

const (
	SubName    = "sub"
	MihomoName = "mihomo"
)

// 用来判断用户是否在运行时更改了覆写订阅的url
var mihomoOverwriteUrl string

// 基础URL配置
var BaseURL string

func UpdateSubStore(yamlData []byte, regionalProxies map[string][]map[string]any) {
	// 调试的时候等一等node启动
	if os.Getenv("SUB_CHECK_SKIP") != "" && config.GlobalConfig.SubStorePort != "" {
		time.Sleep(time.Second * 1)
	}
	// 处理用户输入的格式
	config.GlobalConfig.SubStorePort = formatPort(config.GlobalConfig.SubStorePort)
	// 设置基础URL
	BaseURL = fmt.Sprintf("http://127.0.0.1%s", config.GlobalConfig.SubStorePort)
	if config.GlobalConfig.SubStorePath != "" {
		BaseURL = fmt.Sprintf("%s%s", BaseURL, config.GlobalConfig.SubStorePath)
	}

	if err := checkSub(); err != nil {
		slog.Debug(fmt.Sprintf("检查sub配置文件失败: %v, 正在创建中...", err))
		if err := createSub(yamlData); err != nil {
			slog.Error(fmt.Sprintf("创建sub配置文件失败: %v", err))
			return
		}
	}
	if config.GlobalConfig.MihomoOverwriteUrl == "" {
		slog.Error("mihomo覆写订阅url未设置")
		return
	}
	if err := checkfile(); err != nil {
		slog.Debug(fmt.Sprintf("检查mihomo配置文件失败: %v, 正在创建中...", err))
		if err := createfile(); err != nil {
			slog.Error(fmt.Sprintf("创建mihomo配置文件失败: %v", err))
			return
		}
		mihomoOverwriteUrl = config.GlobalConfig.MihomoOverwriteUrl
	}
	if err := updateSub(yamlData); err != nil {
		slog.Error(fmt.Sprintf("更新sub配置文件失败: %v", err))
		return
	}
	if err := updateRegionalSubs(regionalProxies); err != nil {
		slog.Error(fmt.Sprintf("更新地区订阅失败: %v", err))
		return
	}
	if config.GlobalConfig.MihomoOverwriteUrl != mihomoOverwriteUrl {
		if err := updatefile(); err != nil {
			slog.Error(fmt.Sprintf("更新mihomo配置文件失败: %v", err))
			return
		}
		mihomoOverwriteUrl = config.GlobalConfig.MihomoOverwriteUrl
		slog.Debug("mihomo覆写订阅url已更新")
	}
	slog.Info("substore更新完成")
}

func updateRegionalSubs(regionalProxies map[string][]map[string]any) error {
	regionalContents := make(map[string]string)
	for region, proxies := range regionalProxies {
		if len(proxies) == 0 {
			continue
		}
		body := map[string]any{
			"proxies": proxies,
		}
		jsonBody, err := json.Marshal(body)
		if err != nil {
			slog.Warn(fmt.Sprintf("序列化地区订阅失败: region=%s err=%v", region, err))
			continue
		}
		regionalContents[region] = string(jsonBody)
	}
	existingSubs, err := listSubNames()
	if err != nil {
		return fmt.Errorf("获取现有订阅列表失败: %w", err)
	}

	for region, content := range regionalContents {
		if err := upsertSub(region, content, fmt.Sprintf("地区订阅(%s), subs-check自动维护", region)); err != nil {
			return err
		}
	}

	for name := range existingSubs {
		if name == SubName {
			continue
		}
		if _, ok := regionalContents[name]; ok {
			continue
		}
		if err := deleteSub(name); err != nil {
			return fmt.Errorf("删除空地区订阅失败(%s): %w", name, err)
		}
	}

	if len(regionalContents) > 0 {
		regions := make([]string, 0, len(regionalContents))
		for region := range regionalContents {
			regions = append(regions, region)
		}
		sort.Strings(regions)
		slog.Info("地区订阅更新完成", "regions", strings.Join(regions, ","))
	} else {
		slog.Info("未找到可用地区节点，已清理地区订阅")
	}

	return nil
}

func listSubNames() (map[string]struct{}, error) {
	resp, err := http.Get(fmt.Sprintf("%s/api/subs", BaseURL))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询订阅列表失败, 错误码:%d, 响应:%s", resp.StatusCode, body)
	}

	var result struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if result.Status != "success" {
		return nil, fmt.Errorf("订阅列表返回状态异常: %s", result.Status)
	}

	names := make(map[string]struct{}, len(result.Data))
	for _, item := range result.Data {
		name := strings.ToUpper(strings.TrimSpace(item.Name))
		if name == "" {
			continue
		}
		names[name] = struct{}{}
	}
	return names, nil
}

func upsertSub(name, content, remark string) error {
	if err := checkSubByName(name); err != nil {
		if err := createSubByName(name, content, remark); err != nil {
			return fmt.Errorf("创建地区订阅失败(%s): %w", name, err)
		}
		return nil
	}
	if err := updateSubByName(name, content, remark); err != nil {
		return fmt.Errorf("更新地区订阅失败(%s): %w", name, err)
	}
	return nil
}

func checkSubByName(name string) error {
	resp, err := http.Get(fmt.Sprintf("%s/api/sub/%s", BaseURL, name))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("sub not found")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("查询订阅失败, 错误码:%d, 响应:%s", resp.StatusCode, body)
	}
	var fileResult fileResult
	err = json.Unmarshal(body, &fileResult)
	if err != nil {
		return err
	}
	if fileResult.Status != "success" {
		return fmt.Errorf("获取sub配置文件失败")
	}
	return nil
}

func createSubByName(name, content, remark string) error {
	sub := sub{
		Content: content,
		Name:    name,
		Remark:  remark,
		Source:  "local",
		Process: []map[string]any{{"type": "Quick Setting Operator"}},
	}
	jsonData, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	resp, err := http.Post(fmt.Sprintf("%s/api/subs", BaseURL), "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("创建sub配置文件失败,错误码:%d, 响应:%s", resp.StatusCode, body)
	}
	return nil
}

func updateSubByName(name, content, remark string) error {
	sub := sub{
		Content: content,
		Name:    name,
		Remark:  remark,
		Source:  "local",
		Process: []map[string]any{{"type": "Quick Setting Operator"}},
	}
	jsonData, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/api/sub/%s", BaseURL, name), bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("更新sub配置文件失败,错误码:%d, 响应:%s", resp.StatusCode, body)
	}
	return nil
}

func deleteSub(name string) error {
	req, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/sub/%s", BaseURL, name), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("删除sub配置文件失败,错误码:%d, 响应:%s", resp.StatusCode, body)
	}
	return nil
}

func checkSub() error {
	resp, err := http.Get(fmt.Sprintf("%s/api/sub/%s", BaseURL, SubName))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var fileResult fileResult
	err = json.Unmarshal(body, &fileResult)
	if err != nil {
		return err
	}
	if fileResult.Status != "success" {
		return fmt.Errorf("获取sub配置文件失败")
	}
	return nil
}
func createSub(data []byte) error {
	// sub-store 上传默认限制1MB
	sub := sub{
		Content: string(data),
		Name:    "sub",
		Remark:  "subs-check专用,勿动",
		Source:  "local",
		Process: []map[string]any{
			{
				"type": "Quick Setting Operator",
			},
		},
	}
	json, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	resp, err := http.Post(fmt.Sprintf("%s/api/subs", BaseURL), "application/json", bytes.NewBuffer(json))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("创建sub配置文件失败,错误码:%d", resp.StatusCode)
	}
	return nil
}

func updateSub(data []byte) error {

	sub := sub{
		Content: string(data),
		Name:    "sub",
		Remark:  "subs-check专用,勿动",
		Source:  "local",
		Process: []map[string]any{
			{
				"type": "Quick Setting Operator",
			},
		},
	}
	json, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPatch,
		fmt.Sprintf("%s/api/sub/%s", BaseURL, SubName),
		bytes.NewBuffer(json))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("更新sub配置文件失败,错误码:%d", resp.StatusCode)
	}
	return nil
}

func checkfile() error {
	resp, err := http.Get(fmt.Sprintf("%s/api/wholeFile/%s", BaseURL, MihomoName))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var fileResult fileResult
	err = json.Unmarshal(body, &fileResult)
	if err != nil {
		return err
	}
	if fileResult.Status != "success" {
		return fmt.Errorf("获取mihomo配置文件失败")
	}
	return nil
}
func createfile() error {
	file := file{
		Name: MihomoName,
		Process: []Operator{
			{
				Args: args{
					Content: config.GlobalConfig.MihomoOverwriteUrl,
					Mode:    "link",
				},
				Disabled: false,
				Type:     "Mihomo 覆写",
			},
		},
		Remark:     "subs-check专用,勿动",
		Source:     "sub",
		SourceName: "sub",
		SourceType: "sub",
		Type:       "subscription",
	}
	json, err := json.Marshal(file)
	if err != nil {
		return err
	}
	resp, err := http.Post(fmt.Sprintf("%s/api/wholeFiles", BaseURL), "application/json", bytes.NewBuffer(json))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("创建mihomo配置文件失败,错误码:%d", resp.StatusCode)
	}
	return nil
}

func updatefile() error {
	file := file{
		Name: MihomoName,
		Process: []Operator{
			{
				Args: args{
					Content: config.GlobalConfig.MihomoOverwriteUrl,
					Mode:    "link",
				},
				Disabled: false,
				Type:     "Mihomo 覆写",
			},
		},
		Remark:     "subs-check专用,勿动",
		Source:     "sub",
		SourceName: "sub",
		SourceType: "sub",
		Type:       "subscription",
	}
	json, err := json.Marshal(file)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPatch,
		fmt.Sprintf("%s/api/wholeFile/%s", BaseURL, MihomoName),
		bytes.NewBuffer(json))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("更新mihomo配置文件失败,错误码:%d", resp.StatusCode)
	}
	return nil
}

func formatPort(port string) string {
	// 先去掉空格
	port = strings.TrimSpace(port)

	// 去掉可能存在的协议前缀
	port = strings.TrimPrefix(port, "http://")
	port = strings.TrimPrefix(port, "https://")

	// 去掉主机地址部分，只保留端口
	if strings.Contains(port, ":") {
		parts := strings.Split(port, ":")
		if len(parts) > 1 {
			port = parts[len(parts)-1]
		}
	}

	// 确保有冒号前缀
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	return port
}

func WarpUrl(url string) string {
	url = formatTimePlaceholders(url, time.Now())

	// 如果url中以https://raw.githubusercontent.com开头，那么就使用github代理
	if strings.HasPrefix(url, "https://raw.githubusercontent.com") {
		return config.GlobalConfig.GithubProxy + url
	}
	return url
}

// 动态时间占位符
// 支持在链接中使用时间占位符，会自动替换成当前日期/时间:
// - `{Y}` - 四位年份 (2023)
// - `{m}` - 两位月份 (01-12)
// - `{d}` - 两位日期 (01-31)
// - `{Ymd}` - 组合日期 (20230131)
// - `{Y_m_d}` - 下划线分隔 (2023_01_31)
// - `{Y-m-d}` - 横线分隔 (2023-01-31)
func formatTimePlaceholders(url string, t time.Time) string {
	replacer := strings.NewReplacer(
		"{Y}", t.Format("2006"),
		"{m}", t.Format("01"),
		"{d}", t.Format("02"),
		"{Ymd}", t.Format("20060102"),
		"{Y_m_d}", t.Format("2006_01_02"),
		"{Y-m-d}", t.Format("2006-01-02"),
	)
	return replacer.Replace(url)
}
