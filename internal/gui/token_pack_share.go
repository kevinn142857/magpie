package gui

import "encoding/json"

// tokenPackMCPConfig serializes a recipient's HTTP MCP configuration. The caller
// supplies the public package endpoint and this package's key, never an admin key.
func tokenPackMCPConfig(endpoint, apiKey string) (string, error) {
	type server struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	configuration := struct {
		Servers map[string]server `json:"mcpServers"`
	}{Servers: map[string]server{"magpie-package": {
		Type: "http", URL: endpoint, Headers: map[string]string{"Authorization": "Bearer " + apiKey},
	}}}
	data, err := json.MarshalIndent(configuration, "", "  ")
	return string(data), err
}

// tokenPackShareHTML is self-contained: opening a bearer share link never
// contacts a third party or borrows the administrator's authenticated assets.
const tokenPackShareHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="referrer" content="no-referrer"><title>{{.Pack.Name}} · Magpie</title>
<style nonce="{{.Nonce}}">*{box-sizing:border-box}body{margin:0;background:#f6f7f9;color:#17212e;font:15px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}main{max-width:760px;margin:60px auto;padding:28px}.brand{font-size:13px;letter-spacing:.12em;color:#667384}h1{font-size:32px;line-height:1.25;margin:16px 0 8px}p{color:#667384}.card{background:white;border:1px solid #dfe4eb;border-radius:16px;padding:24px;margin-top:24px}.numbers{display:grid;grid-template-columns:repeat(3,1fr);gap:16px}.call-numbers{border-top:1px solid #dfe4eb;padding-top:20px;margin-top:20px}.numbers span{display:block;color:#667384;font-size:12px}.numbers strong{font-size:24px;font-variant-numeric:tabular-nums}.field{margin:24px 0 0}.label{display:flex;align-items:center;justify-content:space-between;margin-bottom:8px;font-size:13px;font-weight:600}code{display:block;background:#f4f6f9;padding:14px;border-radius:8px;overflow-wrap:anywhere;white-space:pre-wrap;font:13px/1.6 ui-monospace,SFMono-Regular,monospace;user-select:all}button{font:inherit;color:#2459d3;border:1px solid #dbe3f0;background:#fff;padding:5px 12px;border-radius:7px;cursor:pointer}ul{padding-left:20px}small{display:block;margin-top:24px;color:#667384}#copy-status{min-height:24px;color:#2459d3}@media(max-width:520px){main{margin:16px auto;padding:20px}.card{padding:18px}.numbers{grid-template-columns:1fr}.numbers strong{font-size:22px}}</style></head>
<body><main><div class="brand">MAGPIE / TOKEN PACKAGE</div><h1>{{.Pack.Name}}</h1><p>专属模型配置 · 填入支持自定义 API 的客户端即可使用</p>
<section class="card" aria-label="套餐额度"><div class="numbers"><div><span>总额度 · Token</span><strong>{{.Pack.TotalTokens}}</strong></div><div><span>已使用</span><strong>{{.Pack.UsedTokens}}</strong></div><div><span>可用余额</span><strong>{{.Pack.RemainingTokens}}</strong></div></div>{{if .Pack.ReservedTokens}}<p>有 {{.Pack.ReservedTokens}} Token 正在预留中；请求完成或管理员核销后更新。</p>{{end}}<div class="numbers call-numbers" aria-label="搜索与网页读取次数"><div><span>搜索与网页读取 · 总次数</span><strong>{{.Pack.MCPTotalCalls}}</strong></div><div><span>已使用 · 次</span><strong>{{.Pack.MCPUsedCalls}}</strong></div><div><span>剩余 · 次</span><strong>{{.Pack.MCPRemainingCalls}}</strong></div></div><p>与模型 Token 独立计次。{{if .Pack.MCPTotalCalls}}每个接受的搜索或网页读取调用消耗 1 次，网络失败不退还；参数校验失败不扣次。{{else}}此套餐不包含搜索和网页读取，模型 MCP 仍可使用。{{end}}</p></section>
<section class="card" aria-label="API 配置"><div class="field"><div class="label">OpenAI Base URL<button type="button" data-copy="base-url">复制</button></div><code id="base-url">{{.BaseURL}}</code></div><div class="field"><div class="label">Anthropic / Gemini Base URL<button type="button" data-copy="root-url">复制</button></div><code id="root-url">{{.RootURL}}</code></div><div class="field"><div class="label">API Key<button type="button" data-copy="api-key">复制</button></div><code id="api-key">{{.Pack.APIKey}}</code></div><div class="field"><div class="label">可用模型</div><ul>{{range .Pack.Models}}<li><code>{{.}}</code></li>{{end}}</ul></div>{{if .Expiry}}<p>有效期至 {{.Expiry}}</p>{{end}}<p id="copy-status" role="status" aria-live="polite"></p></section>
<section class="card" aria-label="MCP 接入">
<h2>MCP 接入</h2><p>让支持 HTTP MCP 的 Agent 直接使用此套餐的模型与已分配的工具能力，沿用上方同一把 API Key。</p>
<div class="field"><div class="label">套餐 MCP 地址<button type="button" data-copy="mcp-url">复制</button></div><code id="mcp-url">{{.MCPURL}}</code></div>
<div class="field"><div class="label">MCP 配置 JSON<button type="button" data-copy="mcp-config">复制</button></div><code id="mcp-config">{{.MCPConfig}}</code></div>
<p>将配置加入客户端的 MCP 设置；配置文件位置和格式以对应客户端为准。传输方式为 HTTP，认证 Header 为 <code>Authorization: Bearer 套餐 API Key</code>。</p>
<ul><li><code>get_quota</code>查看此套餐的已用、剩余与预留额度。</li><li><code>list_models</code>查看此套餐允许使用的模型。</li><li><code>generate_text</code>使用允许的模型生成文本，消耗同一套餐的 Token 额度。</li>{{if .Pack.MCPTotalCalls}}<li><code>web_search</code>搜索网页，每次接受的工具调用消耗 1 次独立额度。</li><li><code>web_fetch</code>读取网页，每次接受的工具调用消耗 1 次独立额度。</li>{{end}}</ul>
<p>网页搜索需要管理员接入搜索服务，网页读取不依赖搜索供应商；两者均需分配次数。此配置不授予图片生成、插件、本机文件或管理面板权限。</p>
</section>
<small>链接持有者可以查看此套餐的 API Key，请妥善保管。额度按上游报告的输入和输出 Token 计算，包含缓存读取与写入且不重复计数；同一套餐的模型请求依次执行。中断且无法确认用量的请求需要管理员核销。</small></main>
<script nonce="{{.Nonce}}">document.querySelectorAll('[data-copy]').forEach(function(button){button.addEventListener('click',async function(){var text=document.getElementById(button.dataset.copy).textContent;var status=document.getElementById('copy-status');try{if(!navigator.clipboard)throw new Error('clipboard unavailable');await navigator.clipboard.writeText(text);status.textContent='已复制';}catch(error){var selection=window.getSelection();var range=document.createRange();range.selectNodeContents(document.getElementById(button.dataset.copy));selection.removeAllRanges();selection.addRange(range);status.textContent='请手动复制已选中的内容';}});});</script></body></html>`
