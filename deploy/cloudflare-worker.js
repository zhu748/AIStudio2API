/**
 * AIStudio2API — Cloudflare Worker 反向代理
 *
 * 作用:不用 HuggingFace 分配的 *.hf.space 域名,通过 Cloudflare Workers
 * 把自有域名的流量转发到 HF Space,实现:
 *   - 自定义域名(Workers 路由 / Custom Domain)
 *   - 隐藏 HF Space 真实地址,降低被扫描/白嫖风险
 *   - Cloudflare 全球边缘网络加速(HF 域名在部分地区不可达时的中继)
 *   - 可选访问密钥:不懂密钥的请求在边缘直接 401,不打到 HF
 *
 * 部署步骤(详见 README_HF.md 第十章):
 *   1. 复制本文件内容到 Cloudflare Dashboard → Workers → Create Worker
 *   2. 变量 UPSTREAM 改成你的 Space 地址(或用 wrangler vars/secret 下发)
 *   3. (可选)设置 ACCESS_KEY 环境变量,开启边缘鉴权
 *   4. Workers → Settings → Domains & Routes 绑定自有域名
 *      (域名 DNS 必须托管在同一 Cloudflare 账号)
 *
 * 注意事项:
 *   - 流式 SSE/长连接:fetch 的响应体按流式透传,支持 chat 流式输出
 *   - 免费计划:10 万请求/天、单请求 100MB body、CPU 10ms(流式转发够用)
 *   - WebSocket:Workers 原生支持双向透传(管理界面实时日志用)
 */

// UPSTREAM:HF Space 上游地址,以环境变量下发(推荐)或改此处默认值
const DEFAULT_UPSTREAM = "https://YOUR-USERNAME-aistudio2api.hf.space";

// ACCESS_KEY:非空时,请求必须携带以下任一形式才放行(推荐 32+ 随机字符):
//   - Authorization: Bearer <ACCESS_KEY>
//   - X-Access-Key: <ACCESS_KEY>
// 注意:放行后转发时会把 Authorization 头原样透传给 HF(供 PROXY_API_KEY/
// ADMIN_TOKEN Basic Auth 使用),因此边缘密钥与后端密钥可独立设置
const ACCESS_KEY = "";

export default {
  async fetch(request, env) {
    const upstream = (env.UPSTREAM || DEFAULT_UPSTREAM).replace(/\/+$/, "");
    const accessKey = env.ACCESS_KEY || ACCESS_KEY;

    if (accessKey && !authorized(request, accessKey)) {
      return json(401, { error: "unauthorized", hint: "携带 Authorization: Bearer <key> 或 X-Access-Key 头" });
    }

    const incoming = new URL(request.url);
    const target = upstream + incoming.pathname + incoming.search;

    // 构造上游请求:透传方法/头/体
    const upstreamRequest = new Request(target, {
      method: request.method,
      headers: filteredHeaders(request.headers),
      body: ["GET", "HEAD"].includes(request.method) ? undefined : request.body,
      redirect: "manual", // 重定向交回客户端,避免 Worker 内部循环
    });

    // WebSocket 升级请求:直接透传(Workers 原生支持)
    if (request.headers.get("Upgrade") === "websocket") {
      return fetch(upstreamRequest);
    }

    const upstreamResponse = await fetch(upstreamRequest);

    // 透传响应(流式 body 原样返回,不缓冲)
    const headers = new Headers();
    copyHeaders(upstreamResponse.headers, headers);
    headers.delete("content-encoding"); // Worker 已解压时避免长度不匹配
    headers.delete("content-length");
    headers.delete("set-cookie"); // HF 的 cookie 不应写入自有域
    headers.set("x-proxy-by", "cloudflare-worker");

    return new Response(upstreamResponse.body, {
      status: upstreamResponse.status,
      statusText: upstreamResponse.statusText,
      headers,
    });
  },
};

function authorized(request, accessKey) {
  const bearer = (request.headers.get("authorization") || "").replace(/^Bearer\s+/i, "");
  const headerKey = request.headers.get("x-access-key") || "";
  return bearer === accessKey || headerKey === accessKey;
}

function filteredHeaders(headers) {
  const result = new Headers();
  copyHeaders(headers, result);
  // 去掉 Cloudflare 注入的头,避免上游看到重复/矛盾值
  result.delete("cf-connecting-ip");
  result.delete("cf-ipcountry");
  result.delete("cf-ray");
  result.delete("cf-visitor");
  result.delete("cf-worker");
  result.delete("x-forwarded-for");
  result.delete("x-forwarded-proto");
  result.delete("x-real-ip");
  return result;
}

function copyHeaders(source, target) {
  for (const [name, value] of source.entries()) {
    if (value !== undefined) {
      target.set(name, value);
    }
  }
}

function json(status, payload) {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "content-type": "application/json; charset=utf-8" },
  });
}
