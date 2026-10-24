import { useState, type FormEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { useI18n } from "@/components/i18nProvider";
import { listResources, problemText, type Resource } from "@/lib/api/mindlink";
import { useSession } from "@/lib/auth/session";
import { Search } from "lucide-react";

/** 当前账号有权阅读的已发布资讯。 */
export function NewsPage() {
	const { t } = useI18n();
	const session = useSession();
	const [input, setInput] = useState("");
	const [q, setQ] = useState("");
	const [open, setOpen] = useState<Resource | null>(null);
	const resources = useQuery({
		queryKey: ["mindlink", "resources", session?.selectedWorkspace ?? "", q],
		queryFn: () => listResources({ kind: "news", q: q || undefined }),
	});
	const news = resources.data?.filter((item) => item.kind === "news");
	return (
		<div className="flex min-h-0 flex-col gap-4" data-testid="news-page">
			<header className="flex shrink-0 items-center gap-2">
				<h1 className="truncate text-lg font-semibold tracking-tight">{t("ml.news.title")}</h1>
			</header>
			<form
				className="flex max-w-md gap-2"
				role="search"
				onSubmit={(event: FormEvent) => {
					event.preventDefault();
					setQ(input.trim());
				}}
			>
				<Input value={input} onChange={(event) => setInput(event.target.value)} placeholder={t("ml.news.searchPlaceholder")} />
				<Button type="submit" size="sm" variant="outline" aria-label={t("ml.wiki.search")}>
					<Search />
				</Button>
			</form>
			{resources.isError ? (
				<Alert variant="destructive">
					<AlertDescription>{problemText(resources.error, t("ml.news.failed"))}</AlertDescription>
				</Alert>
			) : !news?.length ? (
				<p className="text-muted-foreground py-8 text-center text-xs">{resources.isLoading ? t("eap.loading") : t("ml.news.empty")}</p>
			) : (
				<div className="flex flex-col gap-2" data-testid="news-list">
					{news.map((item) => (
						<Card key={item.id} className="hover:bg-muted/50 cursor-pointer" onClick={() => setOpen(item)}>
							<CardContent className="flex items-start gap-3 py-3">
								<Badge variant="info">{t("ml.kind.news")}</Badge>
								<div className="min-w-0 flex-1">
									<div className="font-medium">{item.title}</div>
									<div className="text-muted-foreground line-clamp-2 text-xs">{item.summary || item.body.slice(0, 120)}</div>
								</div>
								<Badge variant="outline">{t(item.visibility === "team" ? "ml.visibility.team" : "ml.visibility.organization")}</Badge>
							</CardContent>
						</Card>
					))}
				</div>
			)}
			<Dialog open={open !== null} onOpenChange={(next) => !next && setOpen(null)}>
				<DialogContent className="max-w-2xl">
					<DialogHeader>
						<DialogTitle>{open?.title}</DialogTitle>
						<DialogDescription>{open?.summary}</DialogDescription>
					</DialogHeader>
					<div className="max-h-[60vh] overflow-auto text-sm whitespace-pre-wrap">{open?.body}</div>
				</DialogContent>
			</Dialog>
		</div>
	);
}
