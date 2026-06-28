package task

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GMWalletApp/epusdt/model/data"
	"github.com/GMWalletApp/epusdt/model/mdb"
	"github.com/GMWalletApp/epusdt/model/request"
	"github.com/GMWalletApp/epusdt/model/service"
	"github.com/GMWalletApp/epusdt/util/constant"
	"github.com/GMWalletApp/epusdt/util/log"
	"github.com/GMWalletApp/epusdt/util/math"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/shopspring/decimal"
)

const (
	okxExplorerPollInterval   = 10 * time.Second
	okxExplorerDefaultHTMLURL = "https://web3.okx.com/zh-hans/explorer/{chain}/address/{address}/token-transfer"
	okxExplorerUserAgent      = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"
	okxExplorerBrowserTimeout = 30 * time.Second
	okxExplorerBrowserWait    = 8 * time.Second
)

var (
	okxSupportedNetworks = []string{mdb.NetworkBsc, mdb.NetworkEthereum, mdb.NetworkPolygon}
	okxHTMLJSONScriptRe  = regexp.MustCompile(`(?is)<script\b[^>]*type=["']application/json["'][^>]*>(.*?)</script>`)
	okxHTMLScriptRe      = regexp.MustCompile(`(?is)<script\b[^>]*>(.*?)</script>`)
	okxTransferKeyRe     = regexp.MustCompile(`(?i)"(?:txHash|txhash|txId|txid|transactionHash|tranHash|transactionLists)"`)
	okxTxHashRe          = regexp.MustCompile(`(?i)0x[a-f0-9]{64}`)
	okxAddressRe         = regexp.MustCompile(`(?i)0x[a-f0-9]{40}`)
	okxAmountSymbolRe    = regexp.MustCompile(`(?i)(^|[^a-z0-9])([+-]?\d[\d,]*(?:\.\d+)?)\s*(USDT|USDC|DAI|FDUSD|BUSD|ETH|BNB|MATIC|POL)([^a-z0-9]|$)`)
	okxDateTimeRe        = regexp.MustCompile(`\d{4}[-/]\d{2}[-/]\d{2}[ T]\d{2}:\d{2}(?::\d{2})?(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})?`)
)

// OkxObservedTransfer is the normalized token transfer shape consumed by the
// existing order matcher.
type OkxObservedTransfer struct {
	Network              string
	TxHash               string
	ToAddress            string
	TokenSymbol          string
	TokenContractAddress string
	Amount               float64
	BlockTimeMs          int64
	Success              bool
}

type okxAddressPollTarget struct {
	Network string
	Address string
}

type okxExplorerBrowserFetcher func(pageURL string, network string, address string) ([]OkxObservedTransfer, error)

type okxExplorerScanner struct {
	browserFetcher okxExplorerBrowserFetcher
}

var gOkxExplorerJobLock sync.Mutex

type OkxExplorerJob struct{}

func (OkxExplorerJob) Run() {
	gOkxExplorerJobLock.Lock()
	defer gOkxExplorerJobLock.Unlock()

	scanner := okxExplorerScanner{}
	if err := scanner.pollOnce(); err != nil {
		log.Sugar.Warnf("[OKX] poll failed: %v", err)
	}
}

func (s okxExplorerScanner) pollOnce() error {
	nodesByNetwork, err := listOkxNodesByNetwork()
	if err != nil {
		return err
	}
	if len(nodesByNetwork) == 0 {
		return nil
	}

	locks, err := data.ListActiveTransactionLocks(okxSupportedNetworks...)
	if err != nil {
		return err
	}
	targets := uniqueOkxTargets(locks, nodesByNetwork)
	for _, target := range targets {
		node := nodesByNetwork[target.Network]
		transfers, err := s.fetchAddressTransfers(node, target)
		if err != nil {
			recordOkxNodeFailure(target.Network, node, err)
			continue
		}
		data.RecordRpcSuccess(target.Network)
		data.RecordRpcNodeSuccess(node.ID)
		for _, transfer := range transfers {
			processOkxObservedTransfer(transfer)
		}
	}
	return nil
}

func listOkxNodesByNetwork() (map[string]mdb.RpcNode, error) {
	out := make(map[string]mdb.RpcNode)
	for _, network := range okxSupportedNetworks {
		if !data.IsChainEnabled(network) {
			continue
		}
		node, err := data.SelectGeneralRpcNode(network, mdb.RpcNodeTypeOkx)
		if err != nil {
			return nil, err
		}
		if node == nil || node.ID == 0 {
			continue
		}
		if _, ok := okxChainName(network); !ok {
			log.Sugar.Warnf("[OKX-%s] unsupported network", network)
			continue
		}
		out[network] = *node
	}
	return out, nil
}

func uniqueOkxTargets(locks []data.ActiveTransactionLock, nodesByNetwork map[string]mdb.RpcNode) []okxAddressPollTarget {
	seen := make(map[string]struct{})
	targets := make([]okxAddressPollTarget, 0, len(locks))
	for _, lock := range locks {
		network := strings.ToLower(strings.TrimSpace(lock.Network))
		if _, ok := nodesByNetwork[network]; !ok {
			continue
		}
		address := normalizeOkxEvmAddress(lock.Address)
		if address == "" {
			continue
		}
		key := network + "|" + address
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		targets = append(targets, okxAddressPollTarget{Network: network, Address: address})
	}
	return targets
}

func (s okxExplorerScanner) fetchAddressTransfers(node mdb.RpcNode, target okxAddressPollTarget) ([]OkxObservedTransfer, error) {
	pageURL, err := buildOkxExplorerHTMLURL(node.Url, target.Network, target.Address)
	if err != nil {
		return nil, err
	}
	fetcher := s.browserFetcher
	if fetcher == nil {
		fetcher = fetchOkxExplorerTransfersWithBrowser
	}
	transfers, err := fetcher(pageURL, target.Network, target.Address)
	if err != nil {
		return nil, err
	}
	if len(transfers) == 0 {
		log.Sugar.Debugf("[OKX-%s][%s] no token transfers parsed from browser page", target.Network, target.Address)
	}
	return transfers, nil
}

type okxBrowserCapture struct {
	ResponseBodies [][]byte
	DOMHTML        string
	DOMRowsJSON    string
}

type okxCapturedResponse struct {
	RequestID network.RequestID
	URL       string
	MimeType  string
}

func fetchOkxExplorerTransfersWithBrowser(pageURL string, networkName string, address string) ([]OkxObservedTransfer, error) {
	capture, err := captureOkxExplorerPage(pageURL, address)
	if err != nil {
		return nil, err
	}
	return parseOkxExplorerBrowserCapture(capture, networkName, address), nil
}

func captureOkxExplorerPage(pageURL string, address string) (okxBrowserCapture, error) {
	ctx, cancel := context.WithTimeout(context.Background(), okxExplorerBrowserTimeout)
	defer cancel()

	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("blink-settings", "imagesEnabled=false"),
		chromedp.UserAgent(okxExplorerUserAgent),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, allocOpts...)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	var mu sync.Mutex
	responses := make([]okxCapturedResponse, 0, 64)
	seenResponses := make(map[network.RequestID]struct{})
	chromedp.ListenTarget(browserCtx, func(ev interface{}) {
		resp, ok := ev.(*network.EventResponseReceived)
		if !ok || resp == nil || resp.Response == nil {
			return
		}
		if !shouldCaptureOkxBrowserResponse(resp) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := seenResponses[resp.RequestID]; ok {
			return
		}
		seenResponses[resp.RequestID] = struct{}{}
		responses = append(responses, okxCapturedResponse{
			RequestID: resp.RequestID,
			URL:       resp.Response.URL,
			MimeType:  resp.Response.MimeType,
		})
	})

	capture := okxBrowserCapture{}
	err := chromedp.Run(browserCtx,
		network.Enable(),
		chromedp.Navigate(pageURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Sleep(okxExplorerBrowserWait),
		chromedp.OuterHTML("html", &capture.DOMHTML, chromedp.ByQuery),
		chromedp.Evaluate(okxDOMRowsScript(address), &capture.DOMRowsJSON),
		chromedp.ActionFunc(func(ctx context.Context) error {
			mu.Lock()
			copied := append([]okxCapturedResponse(nil), responses...)
			mu.Unlock()
			for _, item := range copied {
				body, err := network.GetResponseBody(item.RequestID).Do(ctx)
				if err != nil || len(bytes.TrimSpace(body)) == 0 {
					continue
				}
				if decoded, err := base64.StdEncoding.DecodeString(string(body)); err == nil && json.Valid(decoded) {
					capture.ResponseBodies = append(capture.ResponseBodies, decoded)
					continue
				}
				capture.ResponseBodies = append(capture.ResponseBodies, body)
			}
			return nil
		}),
	)
	if err != nil {
		return okxBrowserCapture{}, err
	}
	return capture, nil
}

func shouldCaptureOkxBrowserResponse(ev *network.EventResponseReceived) bool {
	if ev.Response == nil {
		return false
	}
	resourceType := ev.Type
	mimeType := strings.ToLower(ev.Response.MimeType)
	rawURL := strings.ToLower(ev.Response.URL)
	if strings.Contains(mimeType, "json") {
		return true
	}
	if resourceType == network.ResourceTypeXHR || resourceType == network.ResourceTypeFetch {
		return true
	}
	return strings.Contains(rawURL, "/api/") &&
		(strings.Contains(rawURL, "transfer") ||
			strings.Contains(rawURL, "transaction") ||
			strings.Contains(rawURL, "address") ||
			strings.Contains(rawURL, "token"))
}

func parseOkxExplorerBrowserCapture(capture okxBrowserCapture, networkName string, address string) []OkxObservedTransfer {
	targetAddress := normalizeOkxEvmAddress(address)
	transfers := make([]OkxObservedTransfer, 0)
	for _, body := range capture.ResponseBodies {
		parsed, err := ParseOkxExplorerTransfers(body, networkName)
		if err != nil {
			continue
		}
		transfers = append(transfers, filterOkxTransfersToAddress(parsed, targetAddress)...)
	}
	if len(transfers) == 0 && strings.TrimSpace(capture.DOMRowsJSON) != "" {
		transfers = append(transfers, filterOkxTransfersToAddress(parseOkxExplorerDOMRows(capture.DOMRowsJSON, networkName, address), targetAddress)...)
	}
	if len(transfers) == 0 && strings.TrimSpace(capture.DOMHTML) != "" {
		parsed, err := ParseOkxExplorerTransfers([]byte(capture.DOMHTML), networkName)
		if err == nil {
			transfers = append(transfers, filterOkxTransfersToAddress(parsed, targetAddress)...)
		}
	}
	return dedupeOkxObservedTransfers(transfers)
}

func filterOkxTransfersToAddress(items []OkxObservedTransfer, targetAddress string) []OkxObservedTransfer {
	if targetAddress == "" {
		return items
	}
	out := make([]OkxObservedTransfer, 0, len(items))
	for _, item := range items {
		if normalizeOkxEvmAddress(item.ToAddress) != targetAddress {
			continue
		}
		out = append(out, item)
	}
	return out
}

func okxDOMRowsScript(address string) string {
	escapedAddress, _ := json.Marshal(strings.ToLower(strings.TrimSpace(address)))
	return fmt.Sprintf(`(() => {
const target = %s;
const candidates = Array.from(document.querySelectorAll('tr,[role="row"],li,div'));
const rows = [];
const seen = new Set();
for (const el of candidates) {
  const text = (el.innerText || el.textContent || '').replace(/\s+/g, ' ').trim();
  if (!text || text.length < 20 || text.length > 4000) continue;
  const lower = text.toLowerCase();
  if (!lower.includes(target) && !/0x[a-f0-9]{64}/i.test(text)) continue;
  if (!/[0-9][0-9,.]*\s*[A-Z][A-Z0-9]{1,15}/.test(text)) continue;
  const links = Array.from(el.querySelectorAll('a[href]')).map(a => ({
    href: a.href || '',
    text: (a.innerText || a.textContent || '').replace(/\s+/g, ' ').trim()
  }));
  const key = text + '|' + links.map(a => a.href).join('|');
  if (seen.has(key)) continue;
  seen.add(key);
  rows.push({ text, links });
  if (rows.length >= 80) break;
}
return JSON.stringify(rows);
})()`, string(escapedAddress))
}

type okxDOMRow struct {
	Text  string       `json:"text"`
	Links []okxDOMLink `json:"links"`
}

type okxDOMLink struct {
	Href string `json:"href"`
	Text string `json:"text"`
}

func parseOkxExplorerDOMRows(raw string, networkName string, address string) []OkxObservedTransfer {
	var rows []okxDOMRow
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil
	}
	transfers := make([]OkxObservedTransfer, 0, len(rows))
	for _, row := range rows {
		transfer := okxTransferFromDOMRow(row, networkName, address)
		if transfer.TxHash == "" || transfer.ToAddress == "" || transfer.TokenSymbol == "" || transfer.Amount <= 0 {
			continue
		}
		transfers = append(transfers, transfer)
	}
	return dedupeOkxObservedTransfers(transfers)
}

func okxTransferFromDOMRow(row okxDOMRow, networkName string, address string) OkxObservedTransfer {
	text := strings.TrimSpace(row.Text)
	transfer := OkxObservedTransfer{
		Network:   strings.ToLower(strings.TrimSpace(networkName)),
		ToAddress: normalizeOkxEvmAddress(address),
		Success:   !okxDOMRowLooksFailed(text),
	}
	for _, link := range row.Links {
		candidate := strings.TrimSpace(link.Href + " " + link.Text)
		if transfer.TxHash == "" {
			transfer.TxHash = strings.ToLower(okxTxHashRe.FindString(candidate))
		}
		if transfer.TokenContractAddress == "" {
			contract := okxFirstNonTxAddress(candidate)
			if contract != "" && normalizeOkxEvmAddress(contract) != transfer.ToAddress {
				transfer.TokenContractAddress = normalizeOkxEvmAddress(contract)
			}
		}
	}
	if transfer.TxHash == "" {
		transfer.TxHash = strings.ToLower(okxTxHashRe.FindString(text))
	}
	addresses := okxAddressRe.FindAllString(okxTxHashRe.ReplaceAllString(text, " "), -1)
	for _, candidate := range addresses {
		normalized := normalizeOkxEvmAddress(candidate)
		if normalized == transfer.ToAddress {
			continue
		}
		if transfer.TokenContractAddress == "" {
			transfer.TokenContractAddress = normalized
		}
	}
	if amount, symbol := okxDOMAmountAndSymbol(text); amount > 0 {
		transfer.Amount = math.MustParsePrecFloat64(amount, data.MaxAmountPrecision)
		transfer.TokenSymbol = strings.ToUpper(symbol)
	}
	transfer.BlockTimeMs = okxDOMRowTimeMs(text)
	return transfer
}

func okxFirstNonTxAddress(text string) string {
	text = okxTxHashRe.ReplaceAllString(text, " ")
	return okxAddressRe.FindString(text)
}

func okxDOMRowLooksFailed(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "failed") ||
		strings.Contains(lower, "fail") ||
		strings.Contains(text, "失败") ||
		strings.Contains(text, "失敗")
}

func okxDOMAmountAndSymbol(text string) (float64, string) {
	matches := okxAmountSymbolRe.FindAllStringSubmatch(text, -1)
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		symbol := strings.ToUpper(strings.TrimSpace(match[3]))
		if symbol == "" || symbol == "USD" || symbol == "CNY" {
			continue
		}
		amount := okxFloat(map[string]interface{}{"amount": match[2]}, "amount")
		if amount <= 0 {
			continue
		}
		return amount, symbol
	}
	return 0, ""
}

func okxDOMRowTimeMs(text string) int64 {
	if raw := okxDateTimeRe.FindString(text); raw != "" {
		return okxParseTimeStringMs(raw)
	}
	return 0
}

func buildOkxExplorerHTMLURL(templateURL, network, address string) (string, error) {
	chain, ok := okxChainName(network)
	if !ok {
		return "", fmt.Errorf("unsupported okx network: %s", network)
	}
	raw := strings.TrimSpace(templateURL)
	if raw == "" {
		raw = okxExplorerDefaultHTMLURL
	}
	raw = strings.ReplaceAll(raw, "{0}", address)
	raw = strings.ReplaceAll(raw, "{chain}", chain)
	raw = strings.ReplaceAll(raw, "{address}", address)

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("invalid okx explorer url")
	}
	return parsed.String(), nil
}

func okxChainName(network string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case mdb.NetworkBsc:
		return "bsc", true
	case mdb.NetworkEthereum:
		return "eth", true
	case mdb.NetworkPolygon:
		return "polygon", true
	default:
		return "", false
	}
}

// ParseOkxExplorerTransfers accepts the OKX/OKLink address token-transfer
// HTML page and extracts transfer-shaped JSON objects embedded in the page.
// JSON bodies are still accepted for tests and for occasional explorer
// responses that serialize state without an HTML shell.
func ParseOkxExplorerTransfers(body []byte, network string) ([]OkxObservedTransfer, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, nil
	}
	lowerHead := strings.ToLower(string(trimmed[:min(len(trimmed), 512)]))
	if trimmed[0] == '<' || strings.Contains(lowerHead, "<html") || strings.Contains(lowerHead, "<script") {
		return parseOkxExplorerHTMLTransfers(trimmed, network)
	}
	return parseOkxExplorerJSONTransfers(trimmed, network)
}

func parseOkxExplorerHTMLTransfers(body []byte, network string) ([]OkxObservedTransfer, error) {
	htmlText := string(body)
	fragments := okxHTMLJSONFragments(htmlText)
	transfers := make([]OkxObservedTransfer, 0)
	for _, fragment := range fragments {
		parsed, err := parseOkxExplorerJSONTransfers([]byte(fragment), network)
		if err != nil {
			if strings.Contains(err.Error(), "okx explorer error") {
				return nil, err
			}
			continue
		}
		transfers = append(transfers, parsed...)
	}
	return dedupeOkxObservedTransfers(transfers), nil
}

func okxHTMLJSONFragments(htmlText string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	add := func(raw string) {
		raw = strings.TrimSpace(stdhtml.UnescapeString(raw))
		if raw == "" {
			return
		}
		if _, ok := seen[raw]; ok {
			return
		}
		seen[raw] = struct{}{}
		out = append(out, raw)
	}

	for _, match := range okxHTMLJSONScriptRe.FindAllStringSubmatch(htmlText, -1) {
		if len(match) > 1 {
			add(match[1])
		}
	}

	for _, match := range okxHTMLScriptRe.FindAllStringSubmatch(htmlText, -1) {
		if len(match) <= 1 {
			continue
		}
		script := strings.TrimSpace(stdhtml.UnescapeString(match[1]))
		if !okxTransferKeyRe.MatchString(script) {
			continue
		}
		for _, fragment := range okxJSONFragmentsAroundTransferKeys(script) {
			add(fragment)
		}
	}
	return out
}

func okxJSONFragmentsAroundTransferKeys(script string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, loc := range okxTransferKeyRe.FindAllStringIndex(script, -1) {
		if len(loc) == 0 {
			continue
		}
		prefix := script[:loc[0]]
		startObj := strings.LastIndex(prefix, "{")
		startArr := strings.LastIndex(prefix, "[")
		start := startObj
		if startArr > start {
			start = startArr
		}
		if start < 0 {
			continue
		}
		fragment := okxExtractBalancedJSON(script, start)
		if fragment == "" {
			continue
		}
		if _, ok := seen[fragment]; ok {
			continue
		}
		seen[fragment] = struct{}{}
		out = append(out, fragment)
	}
	return out
}

func okxExtractBalancedJSON(text string, start int) string {
	if start < 0 || start >= len(text) {
		return ""
	}
	if text[start] != '{' && text[start] != '[' {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return text[start : i+1]
			}
			if depth < 0 {
				return ""
			}
		}
	}
	return ""
}

func parseOkxExplorerJSONTransfers(body []byte, network string) ([]OkxObservedTransfer, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := okxJSONEnvelopeError(value); err != nil {
		return nil, err
	}

	items := collectOkxTransferMaps(value)
	transfers := make([]OkxObservedTransfer, 0, len(items))
	for _, item := range items {
		transfer := okxTransferFromMap(item, network)
		if transfer.TxHash == "" || transfer.ToAddress == "" || transfer.TokenSymbol == "" || transfer.Amount <= 0 {
			continue
		}
		if !transfer.Success {
			continue
		}
		transfers = append(transfers, transfer)
	}
	return dedupeOkxObservedTransfers(transfers), nil
}

func okxJSONEnvelopeError(value interface{}) error {
	root, ok := value.(map[string]interface{})
	if !ok {
		return nil
	}
	code := okxValueString(root["code"])
	if code == "" || code == "0" {
		return nil
	}
	msg := strings.TrimSpace(okxValueString(root["msg"]))
	if msg == "" {
		msg = strings.TrimSpace(okxValueString(root["detailMsg"]))
	}
	if msg == "" {
		msg = fmt.Sprintf("code=%s", code)
	}
	return fmt.Errorf("okx explorer error: %s", msg)
}

func collectOkxTransferMaps(value interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0)
	collectOkxTransferMapsInto(value, &out)
	return out
}

func collectOkxTransferMapsInto(value interface{}, out *[]map[string]interface{}) {
	switch typed := value.(type) {
	case []interface{}:
		for _, item := range typed {
			collectOkxTransferMapsInto(item, out)
		}
	case map[string]interface{}:
		if looksLikeOkxTransferMap(typed) {
			*out = append(*out, typed)
			return
		}
		for _, item := range typed {
			collectOkxTransferMapsInto(item, out)
		}
	}
}

func looksLikeOkxTransferMap(item map[string]interface{}) bool {
	return okxHasAnyKey(item, "txHash", "txhash", "txId", "txid", "hash", "transactionHash", "tranHash") &&
		okxHasAnyKey(item, "to", "toAddress", "toAddr", "receive", "receiver", "recipient", "dst", "targetAddress") &&
		okxHasAnyKey(item, "amount", "value", "quantity", "tokenValue", "realValue", "transactionAmount", "tokenAmount") &&
		(okxHasAnyKey(item, "tokenSymbol", "transactionSymbol", "symbol", "coinSymbol", "coinName", "name", "tokenName", "assetSymbol") ||
			okxHasAnyKey(item, "tokenContractAddress", "contractAddress", "tokenAddress", "tokenContract", "contract") ||
			okxHasAnyKey(item, "token", "tokenInfo", "tokenMeta", "asset", "currency"))
}

func okxHasAnyKey(item map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if _, ok := item[key]; ok {
			return true
		}
	}
	return false
}

func dedupeOkxObservedTransfers(items []OkxObservedTransfer) []OkxObservedTransfer {
	seen := make(map[string]struct{}, len(items))
	transfers := make([]OkxObservedTransfer, 0, len(items))
	for _, item := range items {
		key := strings.Join([]string{
			item.Network,
			strings.ToLower(item.TxHash),
			item.ToAddress,
			item.TokenSymbol,
			item.TokenContractAddress,
			strconv.FormatFloat(item.Amount, 'f', data.MaxAmountPrecision, 64),
		}, "|")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		transfers = append(transfers, item)
	}
	return transfers
}

func okxTransferFromMap(item map[string]interface{}, network string) OkxObservedTransfer {
	amount := okxFloat(item, "amount", "value", "quantity", "tokenValue", "realValue", "transactionAmount", "tokenAmount")
	tokenSymbol := okxString(item, "tokenSymbol", "transactionSymbol", "symbol", "coinSymbol", "coinName", "name", "tokenName", "assetSymbol")
	if tokenSymbol == "" {
		tokenSymbol = okxNestedString(item, []string{"token", "tokenInfo", "tokenMeta", "asset", "currency"}, "symbol", "tokenSymbol", "coinSymbol", "name")
	}
	tokenContract := okxString(item, "tokenContractAddress", "contractAddress", "tokenAddress", "tokenContract", "contract", "address")
	if tokenContract == "" {
		tokenContract = okxNestedString(item, []string{"token", "tokenInfo", "tokenMeta", "asset", "currency"}, "tokenContractAddress", "contractAddress", "tokenAddress", "tokenContract", "contract", "address")
	}
	return OkxObservedTransfer{
		Network:              strings.ToLower(strings.TrimSpace(network)),
		TxHash:               okxString(item, "txHash", "txhash", "txId", "txid", "hash", "transactionHash", "tranHash"),
		ToAddress:            normalizeOkxEvmAddress(okxString(item, "to", "toAddress", "toAddr", "receive", "receiver", "recipient", "dst", "targetAddress")),
		TokenSymbol:          strings.ToUpper(tokenSymbol),
		TokenContractAddress: normalizeOkxEvmAddress(tokenContract),
		Amount:               math.MustParsePrecFloat64(amount, data.MaxAmountPrecision),
		BlockTimeMs:          okxTimeMs(item),
		Success:              okxSuccess(item),
	}
}

func processOkxObservedTransfer(transfer OkxObservedTransfer) {
	if transfer.Network == "" || transfer.TxHash == "" || transfer.ToAddress == "" || transfer.TokenSymbol == "" || transfer.Amount <= 0 {
		return
	}
	if transfer.BlockTimeMs <= 0 {
		log.Sugar.Warnf("[OKX-%s][%s] skip tx=%s without block time", transfer.Network, transfer.ToAddress, transfer.TxHash)
		return
	}
	token, err := resolveOkxTransferToken(transfer)
	if err != nil {
		log.Sugar.Warnf("[OKX-%s][%s] skip tx=%s token lookup: %v", transfer.Network, transfer.ToAddress, transfer.TxHash, err)
		return
	}
	if token == nil || token.ID == 0 {
		log.Sugar.Debugf("[OKX-%s][%s] skip unconfigured token tx=%s symbol=%s contract=%s", transfer.Network, transfer.ToAddress, transfer.TxHash, transfer.TokenSymbol, transfer.TokenContractAddress)
		return
	}
	if token.MinAmount > 0 && transfer.Amount < token.MinAmount {
		log.Sugar.Debugf("[OKX-%s][%s] skip below min amount tx=%s amount=%.8f min=%.8f", transfer.Network, transfer.ToAddress, transfer.TxHash, transfer.Amount, token.MinAmount)
		return
	}
	tokenSym := strings.ToUpper(strings.TrimSpace(token.Symbol))
	tradeID, err := data.GetTradeIdByWalletAddressAndAmountAndToken(transfer.Network, transfer.ToAddress, tokenSym, transfer.Amount)
	if err != nil {
		log.Sugar.Warnf("[OKX-%s][%s] lock lookup tx=%s: %v", transfer.Network, transfer.ToAddress, transfer.TxHash, err)
		return
	}
	if tradeID == "" {
		return
	}
	order, err := data.GetOrderInfoByTradeId(tradeID)
	if err != nil {
		log.Sugar.Warnf("[OKX-%s][%s] load order trade_id=%s tx=%s: %v", transfer.Network, transfer.ToAddress, tradeID, transfer.TxHash, err)
		return
	}
	if strings.ToLower(strings.TrimSpace(order.Network)) != transfer.Network {
		return
	}
	if strings.ToUpper(strings.TrimSpace(order.Token)) != tokenSym {
		return
	}
	if normalizeOkxEvmAddress(order.ReceiveAddress) != transfer.ToAddress {
		return
	}
	if transfer.BlockTimeMs <= order.CreatedAt.TimestampMilli() {
		log.Sugar.Warnf("[OKX-%s][%s] skip tx %s because block time %d is before order create time %d", transfer.Network, transfer.ToAddress, transfer.TxHash, transfer.BlockTimeMs, order.CreatedAt.TimestampMilli())
		return
	}

	err = service.OrderProcessing(&request.OrderProcessingRequest{
		ReceiveAddress:     transfer.ToAddress,
		Token:              tokenSym,
		Network:            transfer.Network,
		TradeId:            tradeID,
		Amount:             transfer.Amount,
		BlockTransactionId: transfer.TxHash,
	})
	if err != nil {
		if errors.Is(err, constant.OrderBlockAlreadyProcess) || errors.Is(err, constant.OrderStatusConflict) {
			return
		}
		log.Sugar.Errorf("[OKX-%s][%s] OrderProcessing trade_id=%s tx=%s: %v", transfer.Network, transfer.ToAddress, tradeID, transfer.TxHash, err)
		return
	}
	log.Sugar.Infof("[OKX-%s][%s] payment processed trade_id=%s tx=%s", transfer.Network, transfer.ToAddress, tradeID, transfer.TxHash)
}

func resolveOkxTransferToken(transfer OkxObservedTransfer) (*mdb.ChainToken, error) {
	if transfer.TokenContractAddress != "" {
		return data.GetEnabledChainTokenByContract(transfer.Network, transfer.TokenContractAddress)
	}
	return data.GetEnabledChainTokenBySymbol(transfer.Network, transfer.TokenSymbol)
}

func recordOkxNodeFailure(network string, node mdb.RpcNode, err error) {
	data.RecordRpcFailure(network)
	failures, cooling := data.RecordRpcNodeFailure(node.ID)
	if cooling {
		log.Sugar.Warnf("[OKX-%s] node reached fail threshold, node=%s err=%v", network, data.RpcNodeLogLabel(node), err)
		return
	}
	log.Sugar.Warnf("[OKX-%s] node failed, node=%s failures=%d/%d err=%v", network, data.RpcNodeLogLabel(node), failures, data.RpcFailoverThreshold, err)
}

func normalizeOkxEvmAddress(address string) string {
	address = strings.TrimSpace(address)
	if address == "" {
		return ""
	}
	return strings.ToLower(address)
}

func okxString(item map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := item[key]; ok {
			out := okxValueString(value)
			if out != "" {
				return out
			}
		}
	}
	return ""
}

func okxValueString(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case map[string]interface{}:
		return okxString(typed, "address", "addr", "hash", "value", "text", "label", "symbol", "name")
	default:
		return ""
	}
}

func okxNestedString(item map[string]interface{}, nestedKeys []string, keys ...string) string {
	for _, nestedKey := range nestedKeys {
		nested, ok := item[nestedKey].(map[string]interface{})
		if !ok {
			continue
		}
		if out := okxString(nested, keys...); out != "" {
			return out
		}
	}
	return ""
}

func okxFloat(item map[string]interface{}, keys ...string) float64 {
	for _, key := range keys {
		value, ok := item[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			parsed, err := decimal.NewFromString(strings.TrimSpace(strings.ReplaceAll(typed, ",", "")))
			if err == nil {
				out, _ := parsed.Float64()
				return out
			}
		case json.Number:
			out, _ := typed.Float64()
			return out
		case float64:
			return typed
		case int:
			return float64(typed)
		case map[string]interface{}:
			if out := okxFloat(typed, "amount", "value", "quantity", "tokenValue", "realValue"); out > 0 {
				return out
			}
		}
	}
	return 0
}

func okxTimeMs(item map[string]interface{}) int64 {
	for _, key := range []string{"blockTime", "blocktime", "timestamp", "time", "transactionTime", "txTime", "createdAt", "date"} {
		value, ok := item[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			typed = strings.TrimSpace(typed)
			if typed == "" {
				continue
			}
			parsed, err := strconv.ParseInt(typed, 10, 64)
			if err == nil {
				return normalizeOkxTimestamp(parsed)
			}
			if parsed := okxParseTimeStringMs(typed); parsed > 0 {
				return parsed
			}
		case json.Number:
			parsed, err := typed.Int64()
			if err == nil {
				return normalizeOkxTimestamp(parsed)
			}
		case float64:
			return normalizeOkxTimestamp(int64(typed))
		}
	}
	return 0
}

func okxParseTimeStringMs(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006/01/02 15:04:05",
		"2006/01/02 15:04",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UnixMilli()
		}
		if parsed, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return parsed.UnixMilli()
		}
	}
	return 0
}

func normalizeOkxTimestamp(value int64) int64 {
	if value <= 0 {
		return 0
	}
	if value < 10_000_000_000 {
		return value * 1000
	}
	return value
}

func okxSuccess(item map[string]interface{}) bool {
	for _, key := range []string{"status", "state", "txStatus", "success", "isSuccess"} {
		value, ok := item[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return typed
		case string:
			switch strings.ToLower(strings.TrimSpace(typed)) {
			case "", "success", "succeed", "completed", "ok", "1", "0x1", "true":
				return true
			case "fail", "failed", "error", "0", "0x0", "false":
				return false
			}
		case float64:
			return typed != 0
		}
	}
	return true
}
