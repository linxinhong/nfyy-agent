import { useState, type FormEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { ArrowRight, BookOpen, Search, Sparkles } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { useI18n } from "@/components/i18nProvider";
import { getWikiTree, listResources, problemText, searchCatalog, searchWiki, type Resource } from "@/lib/api/mindlink";
import { useSession } from "@/lib/auth/session";

/** The homepage only shows content the server has published for this member. */
export function OverviewPage() {
	const { t } = useI18n();
	const session = useSession();
	const navigate = useNavigate();
	const workspace = session?.selectedWorkspace ?? "";
	const [input, setInput] = useState("");
	const [term, setTerm] = useState("");
	const [open, setOpen] = useState<Resource | null>(null);
	const resources = useQuery({
		queryKey: ["mindlink", "resources", workspace, ""],
		queryFn: () => listResources(),
		enabled: Boolean(session),
	});
	const catalog = useQuery({
		queryKey: ["mindlink", "catalog", workspace, ""],
		queryFn: () => searchCatalog(""),
		enabled: Boolean(session),
	});
	const wiki = useQuery({ queryKey: ["mindlink", "wiki-tree", workspace], queryFn: getWikiTree, enabled: Boolean(session) });
	const foundResources = useQuery({
		queryKey: ["mindlink", "home-search-resources", workspace, term],
		queryFn: () => listResources({ q: term }),
		enabled: Boolean(session && term),
	});
	const foundCatalog = useQuery({
		queryKey: ["mindlink", "home-search-catalog", workspace, term],
		queryFn: () => searchCatalog(term),
		enabled: Boolean(session && term),
	});
	const foundWiki = useQuery({
		queryKey: ["mindlink", "home-search-wiki", workspace, term],
		queryFn: () => searchWiki(term),
		enabled: Boolean(session && term),
	});
	const news = (resources.data ?? [])
		.filter((item) => item.kind === "news" && item.status === "approved")
		.sort((a, b) => (b.updated_at ?? "").localeCompare(a.updated_at ?? ""));
	const featured = news[0];
	const found = (foundResources.data ?? []).filter((item) => item.status === "approved");
	const searchError = foundResources.error || foundCatalog.error || foundWiki.error;
	const showResource = (item: Resource) => {
		if (item.kind === "wiki") void navigate({ to: "/portal/wiki", search: { id: item.id } });
		else setOpen(item);
	};

	return (
		<div className="space-y-12 pb-14" data-testid="overview-page">
			<section className="from-primary/10 via-primary/5 rounded-2xl border bg-gradient-to-br to-transparent px-6 py-12 sm:px-10 sm:py-16">
				<div className="max-w-2xl space-y-6">
					<h1 className="text-3xl font-semibold tracking-tight sm:text-4xl">{t("site.home.heading")}</h1>
					<p className="text-muted-foreground">{t("site.home.intro")}</p>
					<form
						role="search"
						className="flex max-w-xl gap-2"
						onSubmit={(event: FormEvent) => {
							event.preventDefault();
							setTerm(input.trim());
						}}
					>
						<Input
							value={input}
							onChange={(event) => setInput(event.target.value)}
							placeholder={t("site.home.searchPlaceholder")}
							aria-label={t("site.home.searchPlaceholder")}
							className="bg-background h-11"
						/>
						<Button type="submit" className="h-11 px-5">
							<Search className="size-4" />
							{t("site.home.search")}
						</Button>
					</form>
				</div>
			</section>

			{term ? (
				<section className="space-y-5" data-testid="home-search-results">
					<div className="flex items-center justify-between gap-3">
						<h2 className="text-xl font-semibold">
							{t("site.home.searchResults")}：{term}
						</h2>
						<Button
							variant="ghost"
							size="sm"
							onClick={() => {
								setTerm("");
								setInput("");
							}}
						>
							{t("site.home.clearSearch")}
						</Button>
					</div>
					{searchError && <ErrorMessage error={searchError} fallback={t("site.home.failed")} />}
					{foundResources.isLoading || foundCatalog.isLoading || foundWiki.isLoading ? (
						<p className="text-muted-foreground py-8">{t("eap.loading")}</p>
					) : (
						<div className="grid gap-8 md:grid-cols-3">
							<SearchGroup title={t("ml.news.title")} empty={t("site.home.noResults")} count={found.length}>
								{found.map((item) => (
									<button
										key={item.id}
										type="button"
										onClick={() => showResource(item)}
										className="hover:text-primary block w-full border-b py-3 text-left text-sm last:border-0"
									>
										{item.title}
									</button>
								))}
							</SearchGroup>
							<SearchGroup title={t("ml.catalog.title")} empty={t("site.home.noResults")} count={foundCatalog.data?.length ?? 0}>
								{foundCatalog.data?.map((item) => (
									<div key={item.id} className="border-b py-3 text-sm last:border-0">
										<div className="font-medium">{item.display_name}</div>
										<p className="text-muted-foreground line-clamp-2 text-xs">{item.description}</p>
									</div>
								))}
							</SearchGroup>
							<SearchGroup title={t("ml.wiki.title")} empty={t("site.home.noResults")} count={foundWiki.data?.length ?? 0}>
								{foundWiki.data?.map((item) => (
									<Link
										key={item.page_id}
										to="/portal/wiki"
										search={{ id: item.page_id }}
										className="hover:text-primary block border-b py-3 text-sm last:border-0"
									>
										{item.title}
									</Link>
								))}
							</SearchGroup>
						</div>
					)}
				</section>
			) : (
				<>
					{!resources.isLoading && !resources.isError && news.length === 0 ? (
						<section className="space-y-4">
							<SectionHeading title={t("site.home.recentNews")} to="/portal/news" more={t("site.home.allNews")} />
							<EmptyBlock text={t("site.home.noNews")} />
						</section>
					) : (
						<section className="grid gap-8 lg:grid-cols-[minmax(0,1.5fr)_minmax(18rem,1fr)]">
							<div className="space-y-4">
								<SectionHeading title={t("site.home.featuredNews")} to="/portal/news" more={t("site.home.allNews")} />
								{resources.isError ? (
									<ErrorMessage error={resources.error} fallback={t("site.home.failed")} />
								) : featured ? (
									<button
										type="button"
										onClick={() => showResource(featured)}
										className="bg-card hover:border-primary/40 group flex min-h-64 w-full flex-col justify-end rounded-xl border p-7 text-left transition-colors"
										data-testid="home-featured-news"
									>
										<span className="text-primary mb-auto text-xs font-medium">{t("site.home.featuredNews")}</span>
										<h3 className="group-hover:text-primary text-2xl font-semibold tracking-tight">{featured.title}</h3>
										<p className="text-muted-foreground mt-3 line-clamp-2 text-sm">{featured.summary || featured.body.slice(0, 160)}</p>
									</button>
								) : (
									<EmptyBlock text={resources.isLoading ? t("eap.loading") : t("site.home.noNews")} />
								)}
							</div>
							<div className="space-y-4">
								<h2 className="text-xl font-semibold">{t("site.home.recentNews")}</h2>
								<div className="bg-card min-h-64 rounded-xl border px-5">
									{news.length > 1 ? (
										news.slice(1, 5).map((item) => (
											<button
												key={item.id}
												type="button"
												onClick={() => showResource(item)}
												className="hover:text-primary block w-full border-b py-4 text-left last:border-0"
											>
												<span className="line-clamp-2 font-medium">{item.title}</span>
												<span className="text-muted-foreground mt-1 block text-xs">
													{item.updated_at ? new Date(item.updated_at).toLocaleDateString() : ""}
												</span>
											</button>
										))
									) : (
										<p className="text-muted-foreground py-8 text-sm">
											{resources.isLoading ? t("eap.loading") : t("site.home.noRecentNews")}
										</p>
									)}
								</div>
							</div>
						</section>
					)}
					<section className="space-y-4">
						<SectionHeading title={t("site.home.capabilities")} to="/portal/catalog" more={t("site.home.allCapabilities")} />
						{catalog.isError ? (
							<ErrorMessage error={catalog.error} fallback={t("site.home.failed")} />
						) : catalog.data?.length ? (
							<div className="grid gap-4 md:grid-cols-3">
								{catalog.data.slice(0, 3).map((item) => (
									<Link
										key={item.id}
										to="/portal/catalog"
										className="bg-card hover:border-primary/40 group rounded-xl border p-6 transition-colors"
									>
										<Sparkles className="text-primary mb-8 size-5" />
										<h3 className="group-hover:text-primary font-semibold">{item.display_name}</h3>
										<p className="text-muted-foreground mt-2 line-clamp-2 text-sm">{item.description}</p>
										<p className="text-muted-foreground mt-5 text-xs">
											{item.publisher} · v{item.version}
										</p>
									</Link>
								))}
							</div>
						) : (
							<EmptyBlock text={catalog.isLoading ? t("eap.loading") : t("site.home.noCapabilities")} />
						)}
					</section>
					<section className="space-y-4">
						<SectionHeading title={t("site.home.wiki")} to="/portal/wiki" more={t("site.home.allWiki")} />
						{wiki.isError ? (
							<ErrorMessage error={wiki.error} fallback={t("site.home.failed")} />
						) : wiki.data?.pages.length ? (
							<div className="grid gap-3 sm:grid-cols-2">
								{wiki.data.pages.slice(0, 5).map((page) => (
									<Link
										key={page.id}
										to="/portal/wiki"
										search={{ id: page.id }}
										className="bg-card hover:border-primary/40 flex items-center gap-3 rounded-lg border px-5 py-4 transition-colors"
									>
										<BookOpen className="text-muted-foreground size-4 shrink-0" />
										<span className="truncate font-medium">{page.title}</span>
										<ArrowRight className="text-muted-foreground ml-auto size-4 shrink-0" />
									</Link>
								))}
							</div>
						) : (
							<EmptyBlock text={wiki.isLoading ? t("eap.loading") : t("site.home.noWiki")} />
						)}
					</section>
				</>
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

function SectionHeading({ title, to, more }: { title: string; to: "/portal/news" | "/portal/wiki" | "/portal/catalog"; more: string }) {
	return (
		<div className="flex items-baseline justify-between gap-3">
			<h2 className="text-xl font-semibold">{title}</h2>
			<Link to={to} className="text-primary inline-flex items-center gap-1 text-sm font-medium hover:underline">
				{more}
				<ArrowRight className="size-4" />
			</Link>
		</div>
	);
}

function SearchGroup({ title, empty, count, children }: { title: string; empty: string; count: number; children: React.ReactNode }) {
	return (
		<div>
			<h3 className="border-b pb-2 font-semibold">{title}</h3>
			{count ? children : <p className="text-muted-foreground py-3 text-sm">{empty}</p>}
		</div>
	);
}

function EmptyBlock({ text }: { text: string }) {
	return <div className="bg-card text-muted-foreground rounded-xl border px-6 py-10 text-sm">{text}</div>;
}

function ErrorMessage({ error, fallback }: { error: unknown; fallback: string }) {
	return (
		<Alert variant="destructive">
			<AlertDescription>{problemText(error, fallback)}</AlertDescription>
		</Alert>
	);
}
