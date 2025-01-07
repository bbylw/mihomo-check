package check

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/mihomo-check/check/platfrom"
	"github.com/bestruirui/mihomo-check/config"
	proxyutils "github.com/bestruirui/mihomo-check/proxy"
	"github.com/bestruirui/mihomo-check/proxy/ipinfo"
	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
	"gopkg.in/yaml.v3"
)

type Result struct {
	Proxy      map[string]any
	Openai     bool
	Youtube    bool
	Netflix    bool
	Google     bool
	Cloudflare bool
	Disney     bool
}

func Check() ([]Result, error) {

	proxyutils.ResetRenameCounter()

	proxies, err := GetProxyFromSubs()

	//清空结果
	results := make([]Result, 0)

	if err != nil {
		return nil, fmt.Errorf("获取节点失败: %w", err)
	}

	log.Infoln("共获取到%d个节点", len(proxies))

	proxies = proxyutils.DeduplicateProxies(proxies)
	log.Infoln("去重后共%d个节点", len(proxies))

	proxyCount := len(proxies)
	threadCount := config.GlobalConfig.Concurrent
	if proxyCount < threadCount {
		threadCount = proxyCount
	}

	// 创建任务通道
	tasks := make(chan map[string]any, proxyCount)
	// 创建结果通道
	resultChan := make(chan Result)

	var progress int32
	var availableCount int32
	var mu sync.Mutex
	done := make(chan bool)

	if config.GlobalConfig.PrintProgress {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					mu.Lock()
					current := progress
					mu.Unlock()

					percent := float64(current) / float64(proxyCount) * 100
					fmt.Printf("\r进度: [%-50s] %.1f%% (%d/%d) 可用: %d",
						strings.Repeat("=", int(percent/2))+">",
						percent,
						current,
						proxyCount,
						availableCount)
					time.Sleep(100 * time.Millisecond)
				}
			}
		}()
	}

	// 启动工作线程
	var wg sync.WaitGroup
	for i := 0; i < threadCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for proxy := range tasks {
				httpClient := CreateClient(proxy)
				if httpClient == nil {
					continue
				}

				mu.Lock()
				progress++
				mu.Unlock()

				cloudflare, err := platfrom.CheckCloudflare(httpClient)
				if err != nil || !cloudflare {
					continue
				}
				google, err := platfrom.CheckGoogle(httpClient)
				if err != nil || !google {
					continue
				}

				openai, err := platfrom.CheckOpenai(httpClient)
				if err != nil {
				}
				youtube, err := platfrom.CheckYoutube(httpClient)
				if err != nil {
				}
				netflix, err := platfrom.CheckNetflix(httpClient)
				if err != nil {
				}
				disney, err := platfrom.CheckDisney(httpClient)
				if err != nil {
				}

				ipfromapi := ipinfo.GetIPaddrFromAPI(httpClient)
				country := ipinfo.GetIPCountrynameFromdb(ipfromapi)
				if country != "" {
					proxy["name"] = country
				} else {
					proxy["name"] = "未识别"
				}
				proxy["name"] = proxyutils.Rename(proxy["name"].(string))

				mu.Lock()
				availableCount++
				mu.Unlock()

				resultChan <- Result{
					Proxy:      proxy,
					Cloudflare: cloudflare,
					Google:     google,
					Openai:     openai,
					Youtube:    youtube,

					Netflix: netflix,
					Disney:  disney,
				}
			}
		}()
	}

	// 发送任务到通道
	go func() {
		for _, proxy := range proxies {
			tasks <- proxy
		}
		close(tasks)
	}()

	// 收集结果
	go func() {
		for result := range resultChan {
			results = append(results, result)
		}
	}()

	wg.Wait()
	close(resultChan)

	if config.GlobalConfig.PrintProgress {
		done <- true
	}

	log.Infoln("共%d个可用节点", len(results))
	return results, nil
}

func GetProxyFromSubs() ([]map[string]any, error) {

	if len(config.GlobalConfig.SubUrls) == 0 {
		log.Errorln("未设置订阅链接")
		os.Exit(1)
	}

	log.Infoln("共设置%d个订阅链接", len(config.GlobalConfig.SubUrls))

	proxies := make([]map[string]any, 0)

	for _, subUrl := range config.GlobalConfig.SubUrls {
		// 添加重试逻辑
		var resp *http.Response
		var err error
		for retries := 0; retries < 30; retries++ {
			resp, err = http.Get(subUrl)
			if err == nil && resp.StatusCode == 200 {
				break
			}
			log.Errorln("获取订阅链接失败: %v,重试次数: %d", err, retries+1)
			time.Sleep(time.Second * time.Duration(retries+1))
		}
		if err != nil {
			return nil, fmt.Errorf("获取订阅链接失败: %w", err)
		}
		defer resp.Body.Close()

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}

		var config map[string]any
		if err := yaml.Unmarshal(data, &config); err != nil {
			log.Errorln("解析订阅链接失败: %v", err)
			log.Errorln("订阅链接: %s", subUrl)
			continue
		}

		// 添加空值检查
		proxyInterface, ok := config["proxies"]
		if !ok || proxyInterface == nil {
			continue
		}

		proxyList, ok := proxyInterface.([]any)
		if !ok {
			continue
		}

		for _, proxy := range proxyList {
			proxyMap, ok := proxy.(map[string]any)
			if !ok {
				continue
			}
			proxies = append(proxies, proxyMap)
		}
	}

	if len(proxies) == 0 {
		return nil, fmt.Errorf("未找到任何可用节点")
	}

	return proxies, nil
}

func CreateClient(mapping map[string]any) *http.Client {
	proxy, err := adapter.ParseProxy(mapping)
	if err != nil {
		return nil
	}

	return &http.Client{
		Timeout: time.Duration(config.GlobalConfig.Timeout) * time.Millisecond,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				var u16Port uint16
				if port, err := strconv.ParseUint(port, 10, 16); err == nil {
					u16Port = uint16(port)
				}
				return proxy.DialContext(ctx, &constant.Metadata{
					Host:    host,
					DstPort: u16Port,
				})
			},
			// 设置连接超时
			IdleConnTimeout: 5 * time.Second,
			// 关闭keepalive
			DisableKeepAlives: true,
		},
	}
}
