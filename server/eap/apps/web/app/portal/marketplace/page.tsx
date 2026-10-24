import { useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { useI18n } from "@/components/i18nProvider";
import { eapRequest, EapProblem } from "@/lib/api/eap";
import { useSession } from "@/lib/auth/session";
import { toast } from "sonner";
import { Search } from "lucide-react";

/**
 * Skill Marketplace —— 服务端目录（TASK-020）：
 * 列表/详情来自 /enterprise/v1/skills 公共契约，与 CLI 同源；评审/发布/
 * 吊销按 maintainer 角色呈现（服务端仍是唯一授权边界）。每个变更展示
 * 服务器结果与 trace id。
 */

interface SkillVersion {
  name: string;
  version: string;
  status: string;
  digest: string;
}

interface Catalog {
  skills: SkillVersion[];
}

async function fetchCatalog(q: string, workspace: string): Promise<Catalog> {
  return eapRequest<Catalog>(
    `/enterprise/v1/skills${q ? `?q=${encodeURIComponent(q)}` : ""}`,
    { headers: workspace ? { "X-Agent-Workspace": workspace } : undefined },
  );
}

export function MarketplacePage() {
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const session = useSession();
  const [query, setQuery] = useState("");
  const [error, setError] = useState<{ code: string; message: string; traceId?: string } | null>(null);

  const catalogQuery = useQuery({
    queryKey: ["eap", "skills", session?.selectedWorkspace, query],
    queryFn: () => fetchCatalog(query, session?.selectedWorkspace ?? ""),
    retry: 0,
  });

  const act = async (name: string, version: string, action: "review" | "release" | "deprecate" | "revoke") => {
    setError(null);
    try {
      await eapRequest(`/enterprise/v1/skills/${name}/versions/${version}/${action}`, {
        method: "POST",
        body: {},
      });
      toast.success(`${action}: ${name}@${version}`);
      void queryClient.invalidateQueries({ queryKey: ["eap", "skills"] });
    } catch (cause) {
      if (cause instanceof EapProblem) {
        setError({ code: cause.code, message: cause.message, traceId: cause.traceId });
      } else {
        setError({ code: "unknown", message: String(cause) });
      }
    }
  };

  const submit = (event: FormEvent) => {
    event.preventDefault();
    void queryClient.invalidateQueries({ queryKey: ["eap", "skills", query] });
  };

  return (
    <div className="flex min-h-0 flex-col gap-4" data-testid="marketplace-page">
      <header className="flex shrink-0 items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2">
          <SidebarTrigger className="shrink-0 md:hidden" />
          <h1 className="truncate text-lg font-semibold tracking-tight">{t("eap.marketplace.title")}</h1>
        </div>
        <form onSubmit={submit} className="flex items-center gap-2">
          <Input
            data-testid="marketplace-search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder={t("eap.marketplace.search")}
            className="h-8 w-48"
          />
          <Button type="submit" variant="outline" size="sm">
            <Search />
          </Button>
        </form>
      </header>

      {error ? (
        <Alert variant="destructive" data-testid="marketplace-error">
          <AlertDescription>
            {error.message}（{t("eap.error.code")}: {error.code}
            {error.traceId ? ` · ${t("eap.error.trace")}: ${error.traceId}` : ""}）
          </AlertDescription>
        </Alert>
      ) : null}

      {catalogQuery.isLoading ? (
        <p className="text-muted-foreground text-xs">{t("eap.loading")}</p>
      ) : catalogQuery.isError ? (
        <Alert variant="destructive">
          <AlertDescription>{String(catalogQuery.error)}</AlertDescription>
        </Alert>
      ) : !catalogQuery.data?.skills?.length ? (
        <p className="text-muted-foreground text-xs">{t("eap.marketplace.empty")}</p>
      ) : (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3" data-testid="marketplace-grid">
          {catalogQuery.data.skills.map((skill) => (
            <Card key={`${skill.name}@${skill.version}`} data-testid={`skill-${skill.name}`}>
              <CardHeader>
                <CardTitle className="flex items-center justify-between gap-2 text-sm">
                  <span className="min-w-0 truncate">{skill.name}</span>
                  <Badge
                    variant={
                      skill.status === "released" ? "success"
                      : skill.status === "approved" ? "info"
                      : skill.status === "pending_review" ? "warning"
                      : "secondary"
                    }
                  >
                    {skill.status}
                  </Badge>
                </CardTitle>
              </CardHeader>
              <CardContent className="flex flex-col gap-2">
                <p className="text-muted-foreground font-mono text-xs">{skill.name}@{skill.version}</p>
                <p className="text-muted-foreground truncate font-mono text-[10px]" title={skill.digest}>
                  sha256:{skill.digest}
                </p>
                <div className="flex flex-wrap gap-1.5">
                  <Button variant="outline" size="sm" onClick={() => void act(skill.name, skill.version, "review")}>
                    {t("eap.marketplace.review")}
                  </Button>
                  <Button variant="outline" size="sm" onClick={() => void act(skill.name, skill.version, "release")}>
                    {t("eap.marketplace.release")}
                  </Button>
                  <Button variant="outline" size="sm" onClick={() => void act(skill.name, skill.version, "deprecate")}>
                    {t("eap.marketplace.deprecate")}
                  </Button>
                  <Button variant="destructive" size="sm" onClick={() => void act(skill.name, skill.version, "revoke")}>
                    {t("eap.marketplace.revoke")}
                  </Button>
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      )}
    </div>
  );
}
