import { useState, type FormEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { useI18n } from "@/components/i18nProvider";
import { problemText, searchCatalog } from "@/lib/api/mindlink";
import { useSession } from "@/lib/auth/session";
import { Search } from "lucide-react";

/**
 * 能力目录 —— 成员可安装到桌面的能力：内置的已签名能力包与 Skill Registry
 * 中已发布的 Skill，合并为一个只读目录（与桌面端同一接口）。Skill 的评审、
 * 发布与吊销仍在 Skill Marketplace 页面。
 */
export function CatalogPage() {
	const { t } = useI18n();
	const session = useSession();
	const [input, setInput] = useState("");
	const [q, setQ] = useState("");
	const catalog = useQuery({
		queryKey: ["mindlink", "catalog", session?.selectedWorkspace ?? "", q],
		queryFn: () => searchCatalog(q),
	});
	return (
		<div className="flex min-h-0 flex-col gap-4" data-testid="catalog-page">
			<header className="flex shrink-0 items-center gap-2">
				<h1 className="truncate text-lg font-semibold tracking-tight">{t("ml.catalog.title")}</h1>
			</header>
			<form
				className="flex max-w-md gap-2"
				role="search"
				onSubmit={(event: FormEvent) => {
					event.preventDefault();
					setQ(input.trim());
				}}
			>
				<Input value={input} onChange={(event) => setInput(event.target.value)} placeholder={t("ml.catalog.searchPlaceholder")} />
				<Button type="submit" size="sm" variant="outline" aria-label={t("ml.wiki.search")}>
					<Search />
				</Button>
			</form>
			{catalog.isError ? (
				<Alert variant="destructive">
					<AlertDescription>{problemText(catalog.error, t("ml.catalog.failed"))}</AlertDescription>
				</Alert>
			) : !catalog.data?.length ? (
				<p className="text-muted-foreground py-8 text-center text-xs">{catalog.isLoading ? t("eap.loading") : t("ml.catalog.empty")}</p>
			) : (
				<div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3" data-testid="catalog-grid">
					{catalog.data.map((item) => (
						<Card key={item.id}>
							<CardHeader>
								<CardTitle className="flex items-center justify-between gap-2 text-sm">
									<span className="truncate">{item.display_name}</span>
									<Badge variant="outline">v{item.version}</Badge>
								</CardTitle>
							</CardHeader>
							<CardContent className="flex flex-col gap-2 text-xs">
								<p className="text-muted-foreground line-clamp-3">{item.description}</p>
								<div className="text-muted-foreground">
									{item.publisher} · {t(item.source.startsWith("registry:") ? "ml.catalog.registry" : "ml.catalog.builtin")}
								</div>
								<div className="flex flex-wrap gap-1">
									{item.components.map((c) => (
										<Badge key={c} variant="secondary">
											{c}
										</Badge>
									))}
									{item.permissions.map((p) => (
										<Badge key={p} variant="warning">
											{p}
										</Badge>
									))}
								</div>
							</CardContent>
						</Card>
					))}
				</div>
			)}
		</div>
	);
}
