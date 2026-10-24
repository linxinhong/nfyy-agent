import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdownMenu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { useI18n } from "@/components/i18nProvider";
import {
	listMySkills,
	problemText,
	publishSkill,
	setSkillEnabled,
	setSkillRequired,
	type SharedSkill,
	type SkillPublish,
} from "@/lib/api/mindlink";
import { usePermissions } from "@/lib/auth/permissions";
import { useSession } from "@/lib/auth/session";
import { toast } from "sonner";
import { MoreHorizontal, RefreshCw, Upload } from "lucide-react";

type Scope = NonNullable<SkillPublish["visibility"]>;
type Draft = { name: string; version: string; visibility: Scope; skillMd: string; files: File[]; protected: boolean };
const emptyDraft: Draft = { name: "", version: "1.0.0", visibility: "team", skillMd: "", files: [], protected: true };

/**
 * 技能 —— 成员的技能架：可用技能、启用开关、必装设置，以及发布新技能。
 *
 * 必装与个人技能总是生效，开关只对其余技能有意义。设为必装需要“设置必装
 * 技能”权限：全所授予可对全所设置，团队内授予只能对本团队设置。发布时
 * SKILL.md 默认受保护，资源文件以文本读入后随包签名，会下载到成员本机。
 */
export function SkillsPage() {
	const { t } = useI18n();
	const session = useSession();
	const queryClient = useQueryClient();
	const workspace = session?.selectedWorkspace ?? "";
	const perms = usePermissions();
	const shelf = useQuery({ queryKey: ["mindlink", "skills", workspace], queryFn: listMySkills });
	const [draft, setDraft] = useState<Draft | null>(null);
	const act = useMutation({
		mutationFn: (run: () => Promise<unknown>) => run(),
		onSuccess: () => {
			toast.success(t("ml.saved"));
			void queryClient.invalidateQueries({ queryKey: ["mindlink", "skills"] });
		},
		onError: (error) => toast.error(problemText(error, t("ml.skills.failed"))),
	});
	const canRequireWorkspace = perms.has("skill.require");
	const canRequireTeam = perms.hasInTeam("skill.require");

	async function submit(event: FormEvent) {
		event.preventDefault();
		if (!draft) return;
		try {
			const resources = await Promise.all(
				draft.files.map(async (file) => ({ path: `resources/${file.name}`, content: await file.text() })),
			);
			const result = await publishSkill(draft.name.trim(), {
				version: draft.version.trim(),
				visibility: draft.visibility,
				protected: draft.protected,
				files: [{ path: "SKILL.md", content: draft.skillMd }, ...resources],
			});
			toast.success(t("ml.skills.published", { status: t(`ml.skillStatus.${result.status}`) }));
			setDraft(null);
			void queryClient.invalidateQueries({ queryKey: ["mindlink", "skills"] });
		} catch (error) {
			toast.error(problemText(error, t("ml.skills.failed")));
		}
	}

	const usable = (skill: SharedSkill) => skill.status === "released" || skill.status === "private";

	return (
		<div className="flex min-h-0 flex-col gap-4" data-testid="skills-page">
			<header className="flex shrink-0 items-center justify-between gap-2">
				<div className="flex min-w-0 items-center gap-2">
					<SidebarTrigger className="shrink-0 md:hidden" />
					<h1 className="truncate text-lg font-semibold tracking-tight">{t("ml.skills.title")}</h1>
				</div>
				<div className="flex gap-2">
					<Button size="sm" variant="outline" onClick={() => void shelf.refetch()} data-testid="skills-refresh">
						<RefreshCw />
						{t("ml.refresh")}
					</Button>
					<Button
						size="sm"
						onClick={() => setDraft({ ...emptyDraft, visibility: perms.groupId ? "team" : "organization" })}
						data-testid="skill-publish-button"
					>
						<Upload />
						{t("ml.skills.publish")}
					</Button>
				</div>
			</header>
			<p className="text-muted-foreground text-sm">{t("ml.skills.hint")}</p>

			<Card>
				<CardContent>
					{shelf.isError ? (
						<Alert variant="destructive">
							<AlertDescription>{problemText(shelf.error, t("ml.skills.failed"))}</AlertDescription>
						</Alert>
					) : !shelf.data?.length ? (
						<p className="text-muted-foreground py-6 text-center text-xs">{shelf.isLoading ? t("eap.loading") : t("ml.skills.empty")}</p>
					) : (
						<Table data-testid="skills-table">
							<TableHeader>
								<TableRow>
									<TableHead>{t("ml.skills.name")}</TableHead>
									<TableHead className="hidden md:table-cell">{t("ml.skills.author")}</TableHead>
									<TableHead>{t("ml.skills.scope")}</TableHead>
									<TableHead>{t("ml.skills.status")}</TableHead>
									<TableHead className="w-20">{t("ml.skills.use")}</TableHead>
									<TableHead className="w-10" />
								</TableRow>
							</TableHeader>
							<TableBody>
								{shelf.data.map((skill) => (
									<TableRow key={`${skill.name}@${skill.version}`} data-testid={`skill-row-${skill.name}`}>
										<TableCell>
											<div className="font-medium">
												{skill.name} <span className="text-muted-foreground text-xs">v{skill.version}</span>{" "}
												{skill.protected && <Badge variant="outline">{t("ml.skills.protected")}</Badge>}
											</div>
											<div className="text-muted-foreground line-clamp-2 text-xs">{skill.description}</div>
											{skill.reject_reason && (
												<div className="text-destructive text-xs">{t("ml.skills.rejected", { reason: skill.reject_reason })}</div>
											)}
										</TableCell>
										<TableCell className="hidden text-sm md:table-cell">{skill.author_name}</TableCell>
										<TableCell>{t(`ml.skillScope.${skill.visibility}`)}</TableCell>
										<TableCell>
											<div className="flex flex-wrap gap-1">
												<Badge variant={skill.status === "rejected" ? "destructive" : "secondary"}>
													{t(`ml.skillStatus.${skill.status}` as "ml.skillStatus.released")}
												</Badge>
												{skill.required && (
													<Badge variant="warning">
														{skill.required_scope === "workspace" ? t("ml.skills.requiredWorkspace") : t("ml.skills.requiredTeam")}
													</Badge>
												)}
											</div>
										</TableCell>
										<TableCell>
											{usable(skill) && (
												<Switch
													checked={skill.active}
													disabled={skill.required || skill.visibility === "personal" || act.isPending}
													title={skill.visibility === "personal" ? t("ml.skills.personalActive") : undefined}
													onCheckedChange={(on) => act.mutate(() => setSkillEnabled(skill.name, on))}
													aria-label={t("ml.skills.use")}
													data-testid={`skill-toggle-${skill.name}`}
												/>
											)}
										</TableCell>
										<TableCell>
											{skill.status === "released" && (canRequireWorkspace || canRequireTeam) && (
												<DropdownMenu>
													<DropdownMenuTrigger asChild>
														<Button size="icon" variant="ghost" className="size-7" aria-label={t("ml.actions")}>
															<MoreHorizontal />
														</Button>
													</DropdownMenuTrigger>
													<DropdownMenuContent align="end">
														{canRequireWorkspace && skill.visibility === "organization" && (
															<>
																<DropdownMenuItem onClick={() => act.mutate(() => setSkillRequired(skill.name, "", true))}>
																	{t("ml.skills.requireWorkspace")}
																</DropdownMenuItem>
																<DropdownMenuItem onClick={() => act.mutate(() => setSkillRequired(skill.name, "", false))}>
																	{t("ml.skills.unrequireWorkspace")}
																</DropdownMenuItem>
															</>
														)}
														{canRequireTeam && (
															<>
																<DropdownMenuItem onClick={() => act.mutate(() => setSkillRequired(skill.name, perms.groupId, true))}>
																	{t("ml.skills.requireTeam")}
																</DropdownMenuItem>
																<DropdownMenuItem onClick={() => act.mutate(() => setSkillRequired(skill.name, perms.groupId, false))}>
																	{t("ml.skills.unrequireTeam")}
																</DropdownMenuItem>
															</>
														)}
													</DropdownMenuContent>
												</DropdownMenu>
											)}
										</TableCell>
									</TableRow>
								))}
							</TableBody>
						</Table>
					)}
				</CardContent>
			</Card>

			<Dialog open={draft !== null} onOpenChange={(open) => !open && setDraft(null)}>
				<DialogContent className="sm:max-w-2xl">
					<DialogHeader>
						<DialogTitle>{t("ml.skills.publish")}</DialogTitle>
						<DialogDescription>{t("ml.skills.publishHint")}</DialogDescription>
					</DialogHeader>
					{draft && (
						<form id="skill-form" className="flex flex-col gap-3" onSubmit={(event) => void submit(event)}>
							<div className="grid gap-3 sm:grid-cols-3">
								<Label className="flex flex-col items-start gap-1">
									{t("ml.skills.slug")}
									<Input
										value={draft.name}
										onChange={(event) => setDraft({ ...draft, name: event.target.value })}
										pattern="[a-z0-9-]{1,64}"
										required
									/>
								</Label>
								<Label className="flex flex-col items-start gap-1">
									{t("ml.skills.version")}
									<Input
										value={draft.version}
										onChange={(event) => setDraft({ ...draft, version: event.target.value })}
										pattern="\d+\.\d+\.\d+"
										required
									/>
								</Label>
								<Label className="flex flex-col items-start gap-1">
									{t("ml.skills.scope")}
									<Select value={draft.visibility} onValueChange={(visibility) => setDraft({ ...draft, visibility: visibility as Scope })}>
										<SelectTrigger className="h-9 w-full">
											<SelectValue />
										</SelectTrigger>
										<SelectContent>
											<SelectItem value="personal">{t("ml.skillScope.personal")}</SelectItem>
											<SelectItem value="team" disabled={!perms.groupId}>
												{t("ml.skillScope.team")}
											</SelectItem>
											<SelectItem value="organization">{t("ml.skillScope.organization")}</SelectItem>
										</SelectContent>
									</Select>
								</Label>
							</div>
							<Label className="flex flex-col items-start gap-1">
								{t("ml.skills.skillMd")}
								<Textarea
									className="min-h-48 font-mono text-xs"
									value={draft.skillMd}
									onChange={(event) => setDraft({ ...draft, skillMd: event.target.value })}
									required
								/>
							</Label>
							<Label className="flex flex-col items-start gap-1">
								{t("ml.skills.resources")}
								<Input type="file" multiple onChange={(event) => setDraft({ ...draft, files: Array.from(event.target.files ?? []) })} />
							</Label>
							<Label className="flex items-center gap-2 font-normal">
								<Checkbox checked={draft.protected} onCheckedChange={(checked) => setDraft({ ...draft, protected: checked === true })} />
								{t("ml.skills.protect")}
							</Label>
						</form>
					)}
					<DialogFooter>
						<Button type="submit" form="skill-form">
							{t("ml.skills.publish")}
						</Button>
					</DialogFooter>
				</DialogContent>
			</Dialog>
		</div>
	);
}
