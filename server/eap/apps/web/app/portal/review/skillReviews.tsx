import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useI18n } from "@/components/i18nProvider";
import { approveSkill, getSkillContent, listSkillReviews, problemText, rejectSkill, type SharedSkill } from "@/lib/api/mindlink";
import { useSession } from "@/lib/auth/session";
import { toast } from "sonner";

/**
 * 技能审核 —— 列出调用者在其范围内可审核的技能版本（服务端已过滤：团队技能
 * 只给本团队有“审核技能”的人，且不含自己发布的）。
 *
 * 审核要读 SKILL.md 原文才能判断，查看会记审计；“通过”即审核加签名发布，
 * “退回”必须写原因，作者在自己的技能架上看到。
 */
export function SkillReviews() {
	const { t } = useI18n();
	const session = useSession();
	const queryClient = useQueryClient();
	const workspace = session?.selectedWorkspace ?? "";
	const pending = useQuery({ queryKey: ["mindlink", "skill-reviews", workspace], queryFn: listSkillReviews });
	const [open, setOpen] = useState<SharedSkill | null>(null);
	const content = useQuery({
		queryKey: ["mindlink", "skill-content", open?.name, open?.version],
		queryFn: () => getSkillContent(open!.name, open!.version),
		enabled: open !== null,
	});
	const act = useMutation({
		mutationFn: (run: () => Promise<unknown>) => run(),
		onSuccess: () => {
			toast.success(t("ml.review.done"));
			setOpen(null);
			void queryClient.invalidateQueries({ queryKey: ["mindlink", "skill-reviews"] });
			void queryClient.invalidateQueries({ queryKey: ["mindlink", "skills"] });
		},
		onError: (error) => toast.error(problemText(error, t("ml.review.failed"))),
	});
	const reject = (skill: SharedSkill) => {
		const reason = window.prompt(t("ml.skills.rejectReason"))?.trim();
		if (reason) act.mutate(() => rejectSkill(skill.name, skill.version, reason));
	};

	return (
		<Card>
			<CardHeader>
				<CardTitle className="text-sm">{t("ml.skills.reviews")}</CardTitle>
			</CardHeader>
			<CardContent>
				{pending.isError ? (
					<Alert variant="destructive">
						<AlertDescription>{problemText(pending.error, t("ml.review.noAccess"))}</AlertDescription>
					</Alert>
				) : !pending.data?.length ? (
					<p className="text-muted-foreground py-6 text-center text-xs">
						{pending.isLoading ? t("eap.loading") : t("ml.skills.reviewsEmpty")}
					</p>
				) : (
					<Table data-testid="skill-reviews">
						<TableHeader>
							<TableRow>
								<TableHead>{t("ml.skills.name")}</TableHead>
								<TableHead className="hidden md:table-cell">{t("ml.skills.author")}</TableHead>
								<TableHead>{t("ml.skills.scope")}</TableHead>
								<TableHead className="w-48 text-right">{t("ml.actions")}</TableHead>
							</TableRow>
						</TableHeader>
						<TableBody>
							{pending.data.map((skill) => (
								<TableRow key={`${skill.name}@${skill.version}`}>
									<TableCell>
										<div className="font-medium">
											{skill.name} <span className="text-muted-foreground text-xs">v{skill.version}</span>
										</div>
										<div className="text-muted-foreground line-clamp-1 text-xs">{skill.description}</div>
									</TableCell>
									<TableCell className="hidden text-sm md:table-cell">{skill.author_name}</TableCell>
									<TableCell>{t(`ml.skillScope.${skill.visibility}`)}</TableCell>
									<TableCell className="space-x-1 text-right">
										<Button size="sm" variant="ghost" onClick={() => setOpen(skill)}>
											{t("ml.skills.view")}
										</Button>
										<Button size="sm" disabled={act.isPending} onClick={() => act.mutate(() => approveSkill(skill))}>
											{t("ml.review.approve")}
										</Button>
										<Button size="sm" variant="outline" disabled={act.isPending} onClick={() => reject(skill)}>
											{t("ml.review.reject")}
										</Button>
									</TableCell>
								</TableRow>
							))}
						</TableBody>
					</Table>
				)}
			</CardContent>
			<Dialog open={open !== null} onOpenChange={(next) => !next && setOpen(null)}>
				<DialogContent className="sm:max-w-3xl">
					<DialogHeader>
						<DialogTitle>
							{open?.name} v{open?.version} {open?.protected && <Badge variant="outline">{t("ml.skills.protected")}</Badge>}
						</DialogTitle>
						<DialogDescription>{open?.description}</DialogDescription>
					</DialogHeader>
					{content.isError ? (
						<Alert variant="destructive">
							<AlertDescription>{problemText(content.error, t("ml.review.noAccess"))}</AlertDescription>
						</Alert>
					) : (
						<div className="flex min-h-0 flex-col gap-2">
							<pre className="bg-muted max-h-96 overflow-auto rounded p-3 text-xs whitespace-pre-wrap" data-testid="skill-md">
								{content.data?.skill_md ?? t("eap.loading")}
							</pre>
							<div className="text-muted-foreground text-xs">
								{t("ml.skills.files")}：{content.data?.files.length ? content.data.files.join("、") : t("ml.skills.noFiles")}
							</div>
						</div>
					)}
					<DialogFooter>
						{open && (
							<>
								<Button variant="outline" disabled={act.isPending} onClick={() => reject(open)}>
									{t("ml.review.reject")}
								</Button>
								<Button disabled={act.isPending} onClick={() => act.mutate(() => approveSkill(open))}>
									{t("ml.review.approve")}
								</Button>
							</>
						)}
					</DialogFooter>
				</DialogContent>
			</Dialog>
		</Card>
	);
}
