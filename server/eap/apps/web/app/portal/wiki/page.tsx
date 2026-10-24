import { useEffect, useMemo, useState, type FormEvent, type MouseEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getRouteApi, useNavigate } from "@tanstack/react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { useI18n } from "@/components/i18nProvider";
import {
	createWikiNode,
	downloadWikiAttachment,
	getWikiPage,
	getWikiPageByPath,
	getWikiTree,
	problemText,
	searchWiki,
	type WikiNode,
	type WikiSearchResult,
} from "@/lib/api/mindlink";
import { usePermissions } from "@/lib/auth/permissions";
import { useSession } from "@/lib/auth/session";
import { toast } from "sonner";
import { BookOpen, ChevronLeft, ChevronRight, FileText, FolderTree, Paperclip, Plus, Search } from "lucide-react";

const route = getRouteApi("/portal/wiki");

const childLevel: Record<string, "book" | "chapter" | undefined> = { shelf: "book", book: "chapter" };

function articleBodyWithoutRepeatedTitle(html: string, title: string): string {
	const document = new DOMParser().parseFromString(html, "text/html");
	const heading = document.body.firstElementChild;
	const plainTitle = title.replace(/^【[^】]+】\s*/, "").trim();
	if (heading?.tagName === "H1" && [title.trim(), plainTitle].includes(heading.textContent?.trim() ?? "")) {
		heading.remove();
		return document.body.innerHTML;
	}
	return html;
}

/**
 * 组织知识库 —— 书架/书/章目录、检索与页面阅读。
 *
 * 页面 HTML 由服务端渲染并按白名单清洗后返回，这里原样展示；站内链接
 * （/wiki/<路径>）在页面内跟随，不整页跳转。目录、检索、正文、链接与
 * 附件都只包含当前账号有权阅读的内容，授权在服务端完成。
 */
export function WikiPageView() {
	const { t } = useI18n();
	const session = useSession();
	const search = route.useSearch();
	const navigate = useNavigate();
	const queryClient = useQueryClient();
	const [query, setQuery] = useState("");
	const [results, setResults] = useState<WikiSearchResult[] | null>(null);
	const [selectedNode, setSelectedNode] = useState("");
	const [draft, setDraft] = useState({ slug: "", title: "" });
	const canManage = usePermissions().has("knowledge.manage");
	const workspace = session?.selectedWorkspace ?? "";

	const tree = useQuery({ queryKey: ["mindlink", "wiki-tree", workspace], queryFn: getWikiTree });
	const page = useQuery({
		queryKey: ["mindlink", "wiki-page", workspace, search.id ?? "", search.path ?? ""],
		queryFn: () => (search.id ? getWikiPage(search.id) : getWikiPageByPath(search.path ?? "")),
		enabled: Boolean(search.id || search.path),
		retry: false,
	});

	const children = useMemo(() => {
		const map = new Map<string, WikiNode[]>();
		for (const node of tree.data?.nodes ?? []) {
			const key = node.parent_id ?? "";
			map.set(key, [...(map.get(key) ?? []), node]);
		}
		return map;
	}, [tree.data]);
	const pagesByNode = useMemo(() => {
		const map = new Map<string, { id: string; title: string }[]>();
		for (const item of tree.data?.pages ?? []) map.set(item.node_id, [...(map.get(item.node_id) ?? []), item]);
		return map;
	}, [tree.data]);
	useEffect(() => {
		const firstPage = tree.data?.pages[0];
		if (search.id || search.path || !firstPage) return;
		void navigate({ to: "/portal/wiki", search: { id: firstPage.id }, replace: true });
	}, [navigate, search.id, search.path, tree.data]);
	const selected = tree.data?.nodes.find((node) => node.id === selectedNode);
	const newLevel = selected ? childLevel[selected.level] : "shelf";

	const open = (next: { id?: string; path?: string }) => void navigate({ to: "/portal/wiki", search: next });

	async function runSearch(event: FormEvent) {
		event.preventDefault();
		if (!query.trim()) {
			setResults(null);
			return;
		}
		try {
			setResults(await searchWiki(query.trim()));
		} catch (error) {
			toast.error(problemText(error, t("ml.wiki.searchFailed")));
		}
	}

	async function createNode(event: FormEvent) {
		event.preventDefault();
		if (!newLevel) return;
		try {
			await createWikiNode({ level: newLevel, parent_id: selected?.id, slug: draft.slug.trim(), title: draft.title.trim() });
			setDraft({ slug: "", title: "" });
			toast.success(t("ml.wiki.nodeCreated"));
			void queryClient.invalidateQueries({ queryKey: ["mindlink", "wiki-tree"] });
		} catch (error) {
			toast.error(problemText(error, t("ml.wiki.nodeFailed")));
		}
	}

	/** 正文里的站内链接在页面内打开。 */
	function followLink(event: MouseEvent<HTMLElement>) {
		const anchor = (event.target as HTMLElement).closest("a");
		const href = anchor?.getAttribute("href") ?? "";
		if (!href.startsWith("/wiki/")) return;
		event.preventDefault();
		open({ path: decodeURIComponent(href.slice("/wiki/".length)) });
	}

	function renderNode(node: WikiNode, depth: number) {
		return (
			<li key={node.id}>
				<button
					type="button"
					style={{ paddingLeft: 10 + depth * 16 }}
					className={`wiki-tree-row ${selectedNode === node.id ? "wiki-tree-row-active" : ""}`}
					onClick={() => setSelectedNode(node.id === selectedNode ? "" : node.id)}
				>
					<FolderTree className="size-4 shrink-0" />
					<span className="truncate">{node.title}</span>
				</button>
				<ul>
					{(children.get(node.id) ?? []).map((child) => renderNode(child, depth + 1))}
					{(pagesByNode.get(node.id) ?? []).map((item) => renderPage(item, depth + 1))}
				</ul>
			</li>
		);
	}

	function renderPage(item: { id: string; title: string }, depth: number) {
		return (
			<li key={item.id}>
				<button
					type="button"
					style={{ paddingLeft: 10 + depth * 16 }}
					className={`wiki-tree-row ${page.data?.id === item.id ? "wiki-tree-row-active" : ""}`}
					onClick={() => open({ id: item.id })}
				>
					<FileText className="size-4 shrink-0" />
					<span className="truncate">{item.title}</span>
				</button>
			</li>
		);
	}

	const current = page.data;
	const orderedPages = tree.data?.pages ?? [];
	const currentIndex = orderedPages.findIndex((item) => item.id === current?.id);
	const previousPage = currentIndex > 0 ? orderedPages[currentIndex - 1] : undefined;
	const nextPage = currentIndex >= 0 ? orderedPages[currentIndex + 1] : undefined;
	return (
		<div className="wiki-workspace" data-testid="wiki-page">
			<h1 className="sr-only">{t("ml.wiki.title")}</h1>
			<div className="wiki-shell">
				<aside className="wiki-sidebar" aria-label={t("ml.wiki.title")}>
					<div className="wiki-sidebar-search">
						<form className="flex gap-2" role="search" onSubmit={(event) => void runSearch(event)}>
							<Input
								value={query}
								onChange={(event) => setQuery(event.target.value)}
								placeholder={t("ml.wiki.searchPlaceholder")}
								aria-label={t("ml.wiki.searchPlaceholder")}
							/>
							<Button type="submit" size="sm" variant="outline" aria-label={t("ml.wiki.search")}>
								<Search />
							</Button>
						</form>
					</div>
					<div className="wiki-sidebar-content">
						{results ? (
							<div className="flex flex-col gap-2" data-testid="wiki-results">
								<Button size="sm" variant="ghost" className="self-start" onClick={() => setResults(null)}>
									{t("ml.wiki.backToTree")}
								</Button>
								{results.length === 0 && <p className="text-muted-foreground text-xs">{t("ml.wiki.noResults")}</p>}
								{results.map((hit) => (
									<button
										key={hit.page_id}
										type="button"
										className="hover:bg-muted rounded p-2 text-left"
										onClick={() => open({ id: hit.page_id })}
									>
										<div className="text-sm font-medium">{hit.title}</div>
										<div className="text-muted-foreground text-xs">{hit.path || t("ml.wiki.unplaced")}</div>
										<div className="text-muted-foreground line-clamp-2 text-xs">{hit.snippet}</div>
									</button>
								))}
							</div>
						) : tree.isError ? (
							<Alert variant="destructive">
								<AlertDescription>{problemText(tree.error, t("ml.wiki.treeFailed"))}</AlertDescription>
							</Alert>
						) : (
							<ul data-testid="wiki-tree">
								{(children.get("") ?? []).map((node) => renderNode(node, 0))}
								{(pagesByNode.get("") ?? []).length > 0 && (
									<li>
										<span className="wiki-tree-group">{t("ml.wiki.unplaced")}</span>
										<ul>{(pagesByNode.get("") ?? []).map((item) => renderPage(item, 1))}</ul>
									</li>
								)}
							</ul>
						)}
						{canManage && newLevel && (
							<form className="flex flex-col gap-2 border-t pt-3" onSubmit={(event) => void createNode(event)}>
								<span className="text-xs font-medium">
									{t("ml.wiki.newNode", { level: t(`ml.wiki.level.${newLevel}` as "ml.wiki.level.shelf") })}
									{selected ? t("ml.wiki.under", { title: selected.title }) : ""}
								</span>
								<Input
									value={draft.slug}
									onChange={(event) => setDraft({ ...draft, slug: event.target.value })}
									placeholder={t("ml.wiki.slug")}
									maxLength={80}
									required
								/>
								<Input
									value={draft.title}
									onChange={(event) => setDraft({ ...draft, title: event.target.value })}
									placeholder={t("ml.wiki.nodeTitle")}
									maxLength={120}
									required
								/>
								<Button type="submit" size="sm" variant="outline">
									<Plus />
									{t("ml.wiki.create")}
								</Button>
							</form>
						)}
					</div>
				</aside>
				<section className="wiki-content" aria-label={t("ml.wiki.title")}>
					<div className="wiki-content-inner" onClick={followLink}>
						{page.isError ? (
							<Alert variant="destructive" data-testid="wiki-page-error">
								<AlertDescription>{t("ml.wiki.pageUnavailable")}</AlertDescription>
							</Alert>
						) : !current ? (
							<div className="text-muted-foreground flex flex-col items-center gap-2 py-16 text-sm">
								<BookOpen className="size-8" />
								{page.isLoading ? t("eap.loading") : t("ml.wiki.pick")}
							</div>
						) : (
							<article className="wiki-document" data-testid="wiki-article">
								<div className="wiki-breadcrumb">
									<span>{t("ml.wiki.title")}</span>
									<ChevronRight className="size-3.5" aria-hidden="true" />
									<span className="truncate">{current.path || t("ml.wiki.unplaced")}</span>
								</div>
								<h2 className="wiki-document-title">{current.title}</h2>
								{current.summary && <p className="wiki-document-summary">{current.summary}</p>}
								<div className="wiki-document-meta">
									<span>{t("ml.wiki.revision", { n: current.revision })}</span>
									{current.updated_at && <time dateTime={current.updated_at}>{new Date(current.updated_at).toLocaleDateString()}</time>}
									{current.tags?.map((tag) => (
										<Badge key={tag} variant="outline">
											#{tag}
										</Badge>
									))}
								</div>
								<div
									className="wiki-body"
									dangerouslySetInnerHTML={{ __html: articleBodyWithoutRepeatedTitle(current.html, current.title) }}
								/>
								{current.attachments.length > 0 && (
									<div className="flex flex-wrap gap-2 border-t pt-3">
										{current.attachments.map((file) => (
											<Button
												key={file.sha256}
												size="sm"
												variant="outline"
												onClick={() =>
													void downloadWikiAttachment(current.id, file).catch(() => toast.error(t("ml.wiki.attachmentFailed")))
												}
											>
												<Paperclip />
												{file.name}（{Math.max(1, Math.round(file.size / 1024))} KB）
											</Button>
										))}
									</div>
								)}
								{current.sources.length > 0 && (
									<p className="text-muted-foreground border-t pt-3 text-xs">
										{t("ml.wiki.sources")}
										{current.sources
											.map((src) => `${src.contribution_id} v${src.version}${src.anchor ? `（${src.anchor}）` : ""}`)
											.join("；")}
									</p>
								)}
								{(previousPage || nextPage) && (
									<nav className="wiki-document-nav" aria-label={t("ml.wiki.title")}>
										{previousPage ? (
											<button type="button" onClick={() => open({ id: previousPage.id })}>
												<ChevronLeft className="size-4 shrink-0" aria-hidden="true" />
												<span>
													<small>上一篇</small>
													<strong>{previousPage.title}</strong>
												</span>
											</button>
										) : (
											<span />
										)}
										{nextPage && (
											<button type="button" onClick={() => open({ id: nextPage.id })}>
												<span>
													<small>下一篇</small>
													<strong>{nextPage.title}</strong>
												</span>
												<ChevronRight className="size-4 shrink-0" aria-hidden="true" />
											</button>
										)}
									</nav>
								)}
							</article>
						)}
					</div>
				</section>
			</div>
		</div>
	);
}
