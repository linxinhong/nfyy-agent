import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useI18n } from "@/components/i18nProvider";
import {
	approveResource,
	decideCurationProposal,
	listCurationProposals,
	problemText,
	rejectResource,
	requestCurationSweep,
	reviewQueue,
	type CurationProposal,
} from "@/lib/api/mindlink";
import { SkillReviews } from "./skillReviews";
import { usePermissions } from "@/lib/auth/permissions";
import { useSession } from "@/lib/auth/session";
import { toast } from "sonner";
import { RefreshCw, Sparkles } from "lucide-react";

const statusVariant: Record<CurationProposal["status"], "warning" | "success" | "secondary" | "destructive" | "info"> = {
	pending: "warning",
	auto_applied: "info",
	accepted: "success",
	rejected: "destructive",
	reverted: "secondary",
};

/**
 * 审核中心 —— 待审核内容（投稿与整理 Agent 的修订）与整理提案。
 *
 * 发布的唯一闸口是这里的“通过”；自动生效的标记、关联、暂停公开可随时撤销。
 * 谁能审核什么由服务端判定：成员看到的是空列表或拒绝，不是隐藏按钮。
 */
export function ReviewPage() {
	const { t } = useI18n();
	const session = useSession();
	const queryClient = useQueryClient();
	const workspace = session?.selectedWorkspace ?? "";
	const canSweep = usePermissions().has("curation.sweep");
	const queue = useQuery({ queryKey: ["mindlink", "review-queue", workspace], queryFn: reviewQueue });
	const proposals = useQuery({ queryKey: ["mindlink", "proposals", workspace], queryFn: listCurationProposals });
	const refresh = () => {
		void queryClient.invalidateQueries({ queryKey: ["mindlink", "review-queue"] });
		void queryClient.invalidateQueries({ queryKey: ["mindlink", "proposals"] });
	};
	const act = useMutation({
		mutationFn: (run: () => Promise<unknown>) => run(),
		onSuccess: () => {
			toast.success(t("ml.review.done"));
			refresh();
		},
		onError: (error) => toast.error(problemText(error, t("ml.review.failed"))),
	});
	const sweep = useMutation({
		mutationFn: requestCurationSweep,
		onSuccess: (result) => toast.success(result.queued ? t("ml.review.sweepQueued") : t("ml.review.sweepOpen")),
		onError: (error) => toast.error(problemText(error, t("ml.review.failed"))),
	});

	return (
		<div className="flex min-h-0 flex-col gap-4" data-testid="review-page">
			<header className="flex shrink-0 items-center justify-between gap-2">
				<div className="flex min-w-0 items-center gap-2">
					<SidebarTrigger className="shrink-0 md:hidden" />
					<h1 className="truncate text-lg font-semibold tracking-tight">{t("ml.review.title")}</h1>
				</div>
				<div className="flex gap-2">
					{canSweep && (
						<Button size="sm" variant="outline" disabled={sweep.isPending} onClick={() => sweep.mutate()} data-testid="sweep-button">
							<Sparkles />
							{t("ml.review.sweep")}
						</Button>
					)}
					<Button size="sm" variant="outline" onClick={refresh}>
						<RefreshCw />
						{t("ml.refresh")}
					</Button>
				</div>
			</header>
			<p className="text-muted-foreground text-xs">{t("ml.review.hint")}</p>

			<Card>
				<CardHeader>
					<CardTitle className="text-sm">{t("ml.review.queue")}</CardTitle>
				</CardHeader>
				<CardContent>
					{queue.isError ? (
						<Alert variant="destructive">
							<AlertDescription>{problemText(queue.error, t("ml.review.noAccess"))}</AlertDescription>
						</Alert>
					) : !queue.data?.length ? (
						<p className="text-muted-foreground py-6 text-center text-xs">
							{queue.isLoading ? t("eap.loading") : t("ml.review.queueEmpty")}
						</p>
					) : (
						<Table data-testid="review-queue">
							<TableHeader>
								<TableRow>
									<TableHead>{t("ml.review.item")}</TableHead>
									<TableHead className="hidden md:table-cell">{t("ml.review.location")}</TableHead>
									<TableHead className="w-40 text-right">{t("ml.actions")}</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{queue.data.map((item) => (
									<TableRow key={`${item.id}-${item.revision}`}>
										<TableCell>
											<div className="font-medium">{item.title}</div>
											<div className="text-muted-foreground line-clamp-1 text-xs">{item.summary || item.body.slice(0, 80)}</div>
										</TableCell>
										<TableCell className="text-muted-foreground hidden text-xs md:table-cell">
											{item.path || t(`ml.kind.${item.kind}` as "ml.kind.wiki")} · {t("ml.wiki.revision", { n: item.revision })}
										</TableCell>
										<TableCell className="space-x-1 text-right">
											<Button size="sm" disabled={act.isPending} onClick={() => act.mutate(() => approveResource(item.id))}>
												{t("ml.review.approve")}
											</Button>
											<Button
												size="sm"
												variant="outline"
												disabled={act.isPending}
												onClick={() => act.mutate(() => rejectResource(item.id))}
											>
												{t("ml.review.reject")}
											</Button>
										</TableCell>
									</TableRow>
								))}
							</TableBody>
						</Table>
					)}
				</CardContent>
			</Card>

			<SkillReviews />

			<Card>
				<CardHeader>
					<CardTitle className="text-sm">{t("ml.review.proposals")}</CardTitle>
				</CardHeader>
				<CardContent>
					{proposals.isError ? (
						<Alert variant="destructive">
							<AlertDescription>{problemText(proposals.error, t("ml.review.noAccess"))}</AlertDescription>
						</Alert>
					) : !proposals.data?.length ? (
						<p className="text-muted-foreground py-6 text-center text-xs">
							{proposals.isLoading ? t("eap.loading") : t("ml.review.proposalsEmpty")}
						</p>
					) : (
						<Table data-testid="proposal-table">
							<TableHeader>
								<TableRow>
									<TableHead>{t("ml.review.kind")}</TableHead>
									<TableHead>{t("ml.review.target")}</TableHead>
									<TableHead className="hidden lg:table-cell">{t("ml.review.rationale")}</TableHead>
									<TableHead className="w-40 text-right">{t("ml.actions")}</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{proposals.data.map((p) => (
									<TableRow key={p.id}>
										<TableCell>
											<div className="text-sm">{t(`ml.proposal.${p.kind}` as "ml.proposal.tag")}</div>
											<Badge variant={statusVariant[p.status]}>{t(`ml.status.${p.status}` as "ml.status.pending")}</Badge>
										</TableCell>
										<TableCell className="text-muted-foreground text-xs">{p.target?.path || p.target?.page_id || "—"}</TableCell>
										<TableCell className="text-muted-foreground hidden max-w-md text-xs lg:table-cell">{p.rationale}</TableCell>
										<TableCell className="space-x-1 text-right">
											{p.status === "pending" && (
												<>
													<Button
														size="sm"
														disabled={act.isPending}
														onClick={() => act.mutate(() => decideCurationProposal(p.id, "accept"))}
													>
														{t("ml.review.approve")}
													</Button>
													<Button
														size="sm"
														variant="outline"
														disabled={act.isPending}
														onClick={() => act.mutate(() => decideCurationProposal(p.id, "reject"))}
													>
														{t("ml.review.reject")}
													</Button>
												</>
											)}
											{p.status === "auto_applied" && (
												<Button
													size="sm"
													variant="outline"
													disabled={act.isPending}
													onClick={() => act.mutate(() => decideCurationProposal(p.id, "revert"))}
												>
													{t("ml.review.revert")}
												</Button>
											)}
										</TableCell>
									</TableRow>
								))}
							</TableBody>
						</Table>
					)}
				</CardContent>
			</Card>
		</div>
	);
}
