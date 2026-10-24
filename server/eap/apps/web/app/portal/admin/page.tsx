import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Input } from "@/components/ui/input";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { useI18n } from "@/components/i18nProvider";
import { eapRequest, EapProblem } from "@/lib/api/eap";
import { toast } from "sonner";

/**
 * Admin Console —— 受治理的平台检查面（TASK-022）：
 * 全部数据来自 /enterprise/v1/admin/* 只读契约（与 CLI/网关同源），分区
 * 展示用户与 Key 态势、Agent 发布、模型路由、用量成本、MCP 工具、知识、
 * DLP 命中与审计事件。服务端是唯一授权边界：非管理员直接调用一律 403；
 * 每次读取本身会写入审计。响应不含任何密钥、凭据引用、签名或存储路径。
 */

const PAGE_SIZE = 20;

const SECTIONS = ["users", "agents", "models", "usage", "mcp", "knowledge", "dlp", "audit"] as const;
type Section = (typeof SECTIONS)[number];

function useAdmin<T>(section: Section, offset: number, filter: string) {
  return useQuery({
    queryKey: ["eap", "admin", section, offset, filter],
    queryFn: () =>
      eapRequest<T>(
        `/enterprise/v1/admin/${section}?limit=${PAGE_SIZE}&offset=${offset}${filter ? `&type=${encodeURIComponent(filter)}` : ""}`,
      ),
    retry: 0,
  });
}

function Th({ children }: { children: React.ReactNode }) {
  return <th className="text-muted-foreground px-2 py-1 text-left font-medium">{children}</th>;
}

function Td({ children }: { children: React.ReactNode }) {
  return <td className="px-2 py-1">{children}</td>;
}

function DataTable({ head, rows }: { head: string[]; rows: React.ReactNode[][] }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b">
            {head.map((h) => (
              <Th key={h}>{h}</Th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, i) => (
            <tr key={i} className="border-b last:border-0" data-testid={`admin-row-${i}`}>
              {row.map((cell, j) => (
                <Td key={j}>{cell}</Td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

interface UsersResponse {
  users: { id: string; email: string; display_name: string; active_keys: number; total_keys: number }[];
}
interface AgentsResponse {
  releases: { client: string; version: string; os: string; arch: string; minimum_version: string; status: string; workspace: string }[];
}
interface ModelsResponse {
  aliases: { alias: string; enabled: boolean; routes: number }[];
  routes: { alias: string; provider: string; model: string; priority: number; enabled: boolean }[];
}
interface UsageResponse {
  rollups: { alias: string; provider: string; calls: number; total_tokens: number; cost_micros: number }[];
  totals: { calls: number; total_tokens: number; cost_micros: number };
}
interface MCPResponse {
  tools: { name: string; server: string; risk_level: string; workspaces: string[] }[];
}
interface KnowledgeResponse {
  documents: { workspace: string; title: string; version: number; status: string }[];
}
interface DLPResponse {
  total: number;
  by_outcome: Record<string, number>;
}
interface DLPRulesResponse {
  rules: { name: string; pattern: string; action: string; enabled: boolean; version: number }[];
}
interface AuditResponse {
  events: { type: string; outcome: string; actor: string | null; workspace: string | null; trace_id: string | null; resource: string | null; time: string }[];
}

export function AdminPage() {
  const { t } = useI18n();
  const [section, setSection] = useState<Section>("users");
  const [offset, setOffset] = useState(0);
  const [filter, setFilter] = useState("");

  const query = useAdmin<Record<string, unknown>>(section, offset, section === "audit" ? filter : "");
  const problem = query.error instanceof EapProblem ? (query.error as EapProblem) : null;

  const pick = <T,>(keys: Section[]): T | null =>
    keys.includes(section) ? (query.data as T) ?? null : null;

  const users = pick<UsersResponse>(["users"]);
  const agents = pick<AgentsResponse>(["agents"]);
  const models = pick<ModelsResponse>(["models"]);
  const usage = pick<UsageResponse>(["usage"]);
  const mcp = pick<MCPResponse>(["mcp"]);
  const knowledge = pick<KnowledgeResponse>(["knowledge"]);
  const dlp = pick<DLPResponse>(["dlp"]);
  const audit = pick<AuditResponse>(["audit"]);

  return (
    <div className="flex min-h-0 flex-col gap-4" data-testid="admin-page">
      <header className="flex shrink-0 items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2">
          <SidebarTrigger className="shrink-0 md:hidden" />
          <h1 className="truncate text-lg font-semibold tracking-tight">{t("eap.admin.title")}</h1>
        </div>
        <div className="flex flex-wrap gap-1">
          {SECTIONS.map((s) => (
            <Button
              key={s}
              size="sm"
              variant={section === s ? "default" : "outline"}
              onClick={() => {
                setSection(s);
                setOffset(0);
              }}
              data-testid={`admin-tab-${s}`}
            >
              {t(`eap.admin.tab.${s}`)}
            </Button>
          ))}
        </div>
      </header>

      {section === "audit" ? (
        <Input
          data-testid="admin-audit-filter"
          value={filter}
          onChange={(e) => {
            setFilter(e.target.value);
            setOffset(0);
          }}
          placeholder={t("eap.admin.auditFilter")}
          className="h-8 w-64"
        />
      ) : null}

      {problem ? (
        <Alert variant="destructive" data-testid="admin-error">
          <AlertDescription>
            {problem.message}（{t("eap.error.code")}: {problem.code}
            {problem.traceId ? ` · ${t("eap.error.trace")}: ${problem.traceId}` : ""}）
          </AlertDescription>
        </Alert>
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">{t(`eap.admin.tab.${section}`)}</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {query.isLoading ? (
            <p className="text-muted-foreground text-xs">{t("eap.loading")}</p>
          ) : (
            <>
              {users && (
                <DataTable
                  head={[t("eap.admin.col.email"), t("eap.admin.col.name"), t("eap.admin.col.activeKeys"), t("eap.admin.col.totalKeys")]}
                  rows={users.users.map((u) => [u.email, u.display_name, String(u.active_keys), String(u.total_keys)])}
                />
              )}
              {agents && (
                <DataTable
                  head={[t("eap.admin.col.client"), "Version", "OS/Arch", t("eap.agentCenter.minimum"), "Status", "Workspace"]}
                  rows={agents.releases.map((r) => [r.client, r.version, `${r.os}/${r.arch}`, r.minimum_version, <Badge key={r.version + r.status} variant="outline">{r.status}</Badge>, r.workspace])}
                />
              )}
              {models && (
                <div className="grid gap-3 md:grid-cols-2">
                  <DataTable head={["Alias", "Enabled", "Routes"]} rows={models.aliases.map((a) => [a.alias, String(a.enabled), String(a.routes)])} />
                  <DataTable head={["Alias", "Provider", "Model", "Priority"]} rows={models.routes.map((r) => [r.alias, r.provider, r.model, String(r.priority)])} />
                </div>
              )}
              {usage && (
                <>
                  <DataTable
                    head={["Alias", "Provider", t("eap.admin.col.calls"), t("eap.admin.col.tokens"), "Cost (micros)"]}
                    rows={usage.rollups.map((r) => [r.alias, r.provider, String(r.calls), String(r.total_tokens), String(r.cost_micros)])}
                  />
                  <p className="text-muted-foreground text-xs" data-testid="admin-usage-totals">
                    {t("eap.admin.col.calls")}: {usage.totals.calls} · {t("eap.admin.col.tokens")}: {usage.totals.total_tokens} · cost_micros: {usage.totals.cost_micros}
                  </p>
                </>
              )}
              {mcp && (
                <DataTable
                  head={["Tool", "Server", "Risk", "Workspaces"]}
                  rows={mcp.tools.map((tool) => [tool.name, tool.server, <Badge key={tool.name} variant="outline">{tool.risk_level}</Badge>, tool.workspaces.join(", ")])}
                />
              )}
              {knowledge && (
                <DataTable
                  head={["Workspace", t("eap.admin.col.title"), "Version", "Status"]}
                  rows={knowledge.documents.map((d, i) => [d.workspace, d.title, String(d.version), <Badge key={i} variant="outline">{d.status}</Badge>])}
                />
              )}
              {dlp && (
                <>
                  <div className="flex flex-wrap gap-2 text-xs" data-testid="admin-dlp-summary">
                    <Badge variant="secondary">total: {dlp.total}</Badge>
                    {Object.entries(dlp.by_outcome).map(([outcome, n]) => (
                      <Badge key={outcome} variant={outcome === "denied" ? "destructive" : "outline"}>
                        {outcome}: {n}
                      </Badge>
                    ))}
                  </div>
                  <DLPRulesPanel key={JSON.stringify(dlp)} />
                  <DLPSandbox />
                </>
              )}
              {audit && (
                <DataTable
                  head={["Time", "Type", "Outcome", "Actor", "Resource", "Trace"]}
                  rows={audit.events.map((e, i) => [
                    <span key={`t${i}`} className="font-mono text-[10px]">{e.time}</span>,
                    e.type,
                    <Badge key={`o${i}`} variant={e.outcome === "denied" ? "destructive" : e.outcome === "allowed" ? "success" : "outline"}>{e.outcome}</Badge>,
                    e.actor ?? "—",
                    e.resource ?? "—",
                    <span key={`tr${i}`} className="truncate font-mono text-[10px]">{e.trace_id ?? "—"}</span>,
                  ])}
                />
              )}
            </>
          )}

          <div className="flex items-center gap-2">
            <Button variant="outline" size="sm" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}>
              {t("eap.admin.prev")}
            </Button>
            <span className="text-muted-foreground text-xs">offset {offset}</span>
            <Button variant="outline" size="sm" onClick={() => setOffset(offset + PAGE_SIZE)}>
              {t("eap.admin.next")}
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

/**
 * DLP 规则管理（TASK-026）：alert = 只告警放行（监测模式）、deny = 告警 +
 * 阻断、redact = 脱敏放行。切换调用 /enterprise/v1/admin/dlp/rules/{name}，
 * 每次变更在网关侧写审计；scanner 缓存 TTL 60s 内生效。
 */
function DLPRulesPanel() {
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const [overrides, setOverrides] = useState<Record<string, { action?: string; enabled?: boolean }>>({});
  const rulesQuery = useQuery({
    queryKey: ["eap", "admin", "dlp-rules"],
    queryFn: () => eapRequest<DLPRulesResponse>("/enterprise/v1/admin/dlp/rules"),
    retry: 0,
  });
  const baseRules: DLPRulesResponse["rules"] = rulesQuery.data?.rules ?? [];
  const rules = baseRules.map((r) => ({
    ...r,
    action: overrides[r.name]?.action ?? r.action,
    enabled: overrides[r.name]?.enabled ?? r.enabled,
  }));

  const update = async (name: string, action: string) => {
    try {
      await eapRequest(`/enterprise/v1/admin/dlp/rules/${encodeURIComponent(name)}`, {
        method: "PATCH",
        body: { action },
      });
      setOverrides((prev) => ({ ...prev, [name]: { ...prev[name], action } }));
      void queryClient.invalidateQueries({ queryKey: ["eap", "admin"] });
      toast.success(`${name}: ${action}`);
    } catch (cause) {
      toast.error(cause instanceof EapProblem ? cause.message : String(cause));
    }
  };

  if (rulesQuery.isLoading) {
    return <p className="text-muted-foreground text-xs">{t("eap.loading")}</p>;
  }
  return (
    <div className="flex flex-col gap-2" data-testid="admin-dlp-rules">
      <p className="text-muted-foreground text-xs">{t("eap.admin.dlpRulesHint")}</p>
      <DataTable
        head={[t("eap.admin.col.name"), "Action", "Enabled", ""]}
        rows={rules.map((r) => [
          <span key={r.name} className="font-mono">{r.name}</span>,
          <Select key={r.name + "-action"} value={r.action} onValueChange={(v) => void update(r.name, v)}>
            <SelectTrigger className="h-7 w-24" data-testid={`dlp-action-${r.name}`}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="alert">alert</SelectItem>
              <SelectItem value="deny">deny</SelectItem>
              <SelectItem value="redact">redact</SelectItem>
              <SelectItem value="tokenize">tokenize</SelectItem>
            </SelectContent>
          </Select>,
          <Badge key={r.name + "-e"} variant={r.enabled ? "success" : "secondary"}>
            {r.enabled ? "on" : "off"}
          </Badge>,
          <span key={r.name + "-v"} className="text-muted-foreground">v{r.version}</span>,
        ])}
      />
    </div>
  );
}

/**
 * DLP 测试沙箱（TASK-031）：贴一段文本，实时看命中哪些规则及脱敏效果。
 * 不落库、不写 vault；仅调用本身有审计（不含内容）。
 */
function DLPSandbox() {
  const { t } = useI18n();
  const [input, setInput] = useState("");
  const [result, setResult] = useState<{ rules: string[]; blocked: boolean; preview: string } | null>(null);
  const [busy, setBusy] = useState(false);

  const run = async () => {
    setBusy(true);
    setResult(null);
    try {
      const res = await eapRequest<{ rules: string[]; blocked: boolean; preview: string }>(
        "/enterprise/v1/admin/dlp/test",
        { method: "POST", body: { content: input } },
      );
      setResult(res);
    } catch (cause) {
      toast.error(cause instanceof EapProblem ? cause.message : String(cause));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-2" data-testid="admin-dlp-sandbox">
      <p className="text-muted-foreground text-xs">{t("eap.admin.dlpSandboxHint")}</p>
      <textarea
        data-testid="dlp-sandbox-input"
        className="border-input bg-background min-h-16 w-full rounded border p-2 font-mono text-xs"
        value={input}
        onChange={(e) => setInput(e.target.value)}
        placeholder="13812345678 / 11010519491231002X / sk-..."
      />
      <Button size="sm" className="w-fit" data-testid="dlp-sandbox-run" disabled={busy || !input} onClick={() => void run()}>
        {t("eap.admin.dlpSandboxRun")}
      </Button>
      {result ? (
        <div className="flex flex-col gap-1" data-testid="dlp-sandbox-result">
          <div className="flex flex-wrap gap-1">
            {result.rules.length === 0 ? (
              <Badge variant="secondary">no hits</Badge>
            ) : (
              result.rules.map((r) => (
                <Badge key={r} variant={result.blocked ? "destructive" : "outline"}>
                  {r}
                </Badge>
              ))
            )}
          </div>
          <pre className="bg-muted overflow-x-auto rounded p-2 font-mono text-[11px]">{result.preview}</pre>
        </div>
      ) : null}
    </div>
  );
}
