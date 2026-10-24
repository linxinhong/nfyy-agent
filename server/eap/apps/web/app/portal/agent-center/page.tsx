import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
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
import { SidebarTrigger } from "@/components/ui/sidebar";
import { useI18n } from "@/components/i18nProvider";
import { eapRequest, EapProblem } from "@/lib/api/eap";
import { useSession } from "@/lib/auth/session";
import { buildClientConfig, downloadClientConfig } from "@/lib/utils/clientConfig";
import { toast } from "sonner";
import { Download } from "lucide-react";

/**
 * Agent Center —— 公司允许使用的 Agent 客户端（TASK-020）：
 * 发布版本来自 /enterprise/v1/agents 服务端契约（与 CLI 同一数据面），
 * 平台由浏览器探测并可通过选择器切换（每次请求都携带 os/arch，服务端
 * 契约不允许未声明平台的回退）。员工下载的客户端配置只含端点与说明，
 * 不含密钥；发布字节、存储位置与签名材料绝不出现在此页面。
 */

interface AgentRelease {
  client: string;
  version: string;
  os: string;
  arch: string;
  minimum_version: string;
  notes: string;
  digest: string;
}

const PLATFORMS = [
  { os: "darwin", arch: "arm64", label: "macOS · Apple Silicon" },
  { os: "darwin", arch: "amd64", label: "macOS · Intel" },
  { os: "windows", arch: "amd64", label: "Windows · x64" },
  { os: "linux", arch: "amd64", label: "Linux · x64" },
] as const;

/** 浏览器平台探测：仅作为选择器的初始值，安装匹配仍以服务端契约为准。 */
function detectPlatform(): string {
  const nav = navigator as Navigator & { userAgentData?: { platform?: string } };
  const raw = nav.userAgentData?.platform ?? navigator.platform ?? "";
  if (/mac/i.test(raw)) return "darwin-arm64";
  if (/win/i.test(raw)) return "windows-amd64";
  if (/linux/i.test(raw)) return "linux-amd64";
  return "darwin-arm64";
}

async function fetchReleases(client: string, os: string, arch: string, workspace: string): Promise<{ releases: AgentRelease[] }> {
  return eapRequest<{ releases: AgentRelease[] }>(
    `/enterprise/v1/agents/${encodeURIComponent(client)}?os=${os}&arch=${arch}`,
    { headers: workspace ? { "X-Agent-Workspace": workspace } : undefined },
  );
}

export function AgentCenterPage() {
  const { t } = useI18n();
  const session = useSession();
  const [platform, setPlatform] = useState<string>(() => detectPlatform());
  const [os = "darwin", arch = "arm64"] = platform.split("-");

  // 服务端目录：列出公司登记的客户端在当前选择平台上的已发布版本。
  const harnessQuery = useQuery({
    queryKey: ["eap", "agents", "harness", os, arch, session?.selectedWorkspace],
    queryFn: () => fetchReleases("harness", os, arch, session?.selectedWorkspace ?? ""),
    retry: 0,
  });
  const codexQuery = useQuery({
    queryKey: ["eap", "agents", "codex", os, arch, session?.selectedWorkspace],
    queryFn: () => fetchReleases("codex", os, arch, session?.selectedWorkspace ?? ""),
    retry: 0,
  });

  const queries = [harnessQuery, codexQuery];
  const failed = queries.find((q) => q.error instanceof EapProblem);
  const shownError = failed ? (failed.error as EapProblem) : null;

  const downloadFor = () => {
    downloadClientConfig(buildClientConfig(window.location.origin, session?.selectedWorkspace ?? ""));
    toast.success(t("eap.keys.configDownloaded"));
  };

  return (
    <div className="flex min-h-0 flex-col gap-4" data-testid="agent-center-page">
      <header className="flex shrink-0 items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2">
          <SidebarTrigger className="shrink-0 md:hidden" />
          <h1 className="truncate text-lg font-semibold tracking-tight">{t("eap.agentCenter.title")}</h1>
        </div>
        <Select value={platform} onValueChange={setPlatform}>
          <SelectTrigger className="h-8 w-52" data-testid="agent-platform-select">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {PLATFORMS.map((p) => (
              <SelectItem key={p.os + p.arch} value={`${p.os}-${p.arch}`}>
                {p.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </header>

      <p className="text-muted-foreground text-xs">{t("eap.agentCenter.hint")}</p>

      <Card data-testid="cli-install-card">
        <CardHeader>
          <CardTitle className="flex items-center justify-between gap-2 text-sm">
            <span>{t("eap.agentCenter.cli.title")}</span>
            <Badge variant="outline">curl | bash</Badge>
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-2 text-xs">
          <p className="text-muted-foreground">{t("eap.agentCenter.cli.hint")}</p>
          <div className="flex items-center gap-2">
            <code className="bg-muted flex-1 overflow-x-auto rounded px-2 py-1.5 font-mono text-[11px]" data-testid="cli-install-command">
              curl -fsSL {window.location.origin}/cli/install.sh | bash
            </code>
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                void navigator.clipboard.writeText(`curl -fsSL ${window.location.origin}/cli/install.sh | bash`);
                toast.success(t("eap.agentCenter.cli.copied"));
              }}
              data-testid="copy-cli-install"
            >
              {t("eap.agentCenter.cli.copy")}
            </Button>
          </div>
          <p className="text-muted-foreground">{t("eap.agentCenter.cli.next")}</p>
        </CardContent>
      </Card>

      {shownError ? (
        <Alert variant="destructive" data-testid="agent-center-error">
          <AlertDescription>
            {shownError.message}（{t("eap.error.code")}: {shownError.code}
            {"traceId" in shownError && shownError.traceId ? ` · ${t("eap.error.trace")}: ${shownError.traceId}` : ""}）
          </AlertDescription>
        </Alert>
      ) : null}

      <div className="grid min-h-0 grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
        {["harness", "codex"].map((client) => {
          const query = client === "harness" ? harnessQuery : codexQuery;
          const latest = query.data?.releases?.[0];
          return (
            <Card key={client} data-testid={`agent-client-${client}`}>
              <CardHeader>
                <CardTitle className="flex items-center justify-between gap-2 text-sm">
                  <span className="min-w-0 truncate">{client}</span>
                  {latest ? (
                    <Badge variant="success">{t("eap.agentCenter.allowed")}</Badge>
                  ) : (
                    <Badge variant="secondary">{t("eap.agentCenter.blocked")}</Badge>
                  )}
                </CardTitle>
              </CardHeader>
              <CardContent className="flex flex-col gap-2 text-xs">
                {query.isLoading ? (
                  <p className="text-muted-foreground">{t("eap.loading")}</p>
                ) : latest ? (
                  <>
                    <div className="flex justify-between gap-2">
                      <span className="text-muted-foreground">{t("eap.agentCenter.recommended")}</span>
                      <span className="font-mono" data-testid={`agent-version-${client}`}>{latest.version}</span>
                    </div>
                    <div className="flex justify-between gap-2">
                      <span className="text-muted-foreground">{t("eap.agentCenter.minimum")}</span>
                      <span className="font-mono">{latest.minimum_version}</span>
                    </div>
                    <div className="flex flex-wrap gap-1">
                      <Badge variant="outline">{latest.os}/{latest.arch}</Badge>
                      <Badge variant="outline">{t("eap.agentCenter.downloadable")}</Badge>
                    </div>
                    {latest.notes ? <p className="text-muted-foreground">{latest.notes}</p> : null}
                  </>
                ) : (
                  <p className="text-muted-foreground">{t("eap.agentCenter.empty")}</p>
                )}
                <Button
                  variant="outline"
                  size="sm"
                  className="mt-auto w-fit"
                  data-testid={`download-config-${client}`}
                  onClick={downloadFor}
                >
                  <Download />
                  {t("eap.keys.downloadConfig")}
                </Button>
              </CardContent>
            </Card>
          );
        })}
      </div>
    </div>
  );
}
