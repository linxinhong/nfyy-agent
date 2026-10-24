import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useI18n } from "@/components/i18nProvider";
import { RolePicker } from "@/components/rolePicker";
import {
	createGroup,
	createMember,
	deleteGroup,
	listGroups,
	listMembers,
	listRoles,
	problemText,
	removeMember,
	renameGroup,
	updateMember,
	type Member,
	type MemberCreated,
	type MemberCreateRequest,
} from "@/lib/api/mindlink";
import { useSession } from "@/lib/auth/session";
import { toast } from "sonner";
import { Plus, Trash2, UserPlus } from "lucide-react";

const NO_GROUP = "__none__";

/**
 * 成员与团队 —— 在当前 Workspace 内建团队、建成员、分配全所角色与团队角色。
 *
 * 角色来自 Workspace 的角色目录（可自定义）；分配或撤下一个角色需要自己拥有
 * 它包含的全部权限，否则网关拒绝，页面只提示失败原因。团队角色只在成员所在
 * 团队内生效，换团队时随之清空。
 *
 * 新成员的一次性密码只在创建结果里出现一次（关闭对话框即消失，不写任何
 * 存储），首次登录必须改密。移出成员会让其会话与 Key 立即失效。
 */
export function MembersPage() {
	const { t } = useI18n();
	const session = useSession();
	const queryClient = useQueryClient();
	const workspace = session?.selectedWorkspace ?? "";
	const members = useQuery({ queryKey: ["mindlink", "members", workspace], queryFn: listMembers });
	const groups = useQuery({ queryKey: ["mindlink", "groups", workspace], queryFn: listGroups });
	const catalog = useQuery({ queryKey: ["mindlink", "roles", workspace], queryFn: listRoles });
	const workspaceRoles = (catalog.data?.roles ?? []).filter((role) => role.scope === "workspace");
	const teamRoles = (catalog.data?.roles ?? []).filter((role) => role.scope === "team");
	const [groupName, setGroupName] = useState("");
	const [createOpen, setCreateOpen] = useState(false);
	const [created, setCreated] = useState<MemberCreated | null>(null);
	const emptyDraft = { username: "", display_name: "", email: "", roles: ["developer"], group_id: "", team_roles: [] };
	const [draft, setDraft] = useState<MemberCreateRequest & { roles: string[]; team_roles: string[] }>(emptyDraft);
	const refresh = () => {
		void queryClient.invalidateQueries({ queryKey: ["mindlink", "members"] });
		void queryClient.invalidateQueries({ queryKey: ["mindlink", "groups"] });
	};
	const act = useMutation({
		mutationFn: (run: () => Promise<unknown>) => run(),
		onSuccess: () => {
			toast.success(t("ml.saved"));
			refresh();
		},
		onError: (error) => toast.error(problemText(error, t("ml.members.failed"))),
	});

	async function submitMember(event: FormEvent) {
		event.preventDefault();
		try {
			const result = await createMember({
				...draft,
				username: draft.username.trim(),
				display_name: draft.display_name.trim(),
				email: draft.email?.trim() || undefined,
				group_id: draft.group_id || undefined,
				team_roles: draft.group_id ? draft.team_roles : [],
			});
			setCreateOpen(false);
			setCreated(result);
			setDraft(emptyDraft);
			refresh();
		} catch (error) {
			toast.error(problemText(error, t("ml.members.failed")));
		}
	}

	const groupOptions = groups.data ?? [];
	const groupSelect = (value: string, onChange: (value: string) => void, testId?: string) => (
		<Select value={value || NO_GROUP} onValueChange={(next) => onChange(next === NO_GROUP ? "" : next)}>
			<SelectTrigger className="h-8 w-36" data-testid={testId}>
				<SelectValue />
			</SelectTrigger>
			<SelectContent>
				<SelectItem value={NO_GROUP}>{t("ml.members.noGroup")}</SelectItem>
				{groupOptions.map((group) => (
					<SelectItem key={group.id} value={group.id}>
						{group.name}
					</SelectItem>
				))}
			</SelectContent>
		</Select>
	);
	// 只发送改动的字段：未改动的角色不需要（也不该要求）分配者拥有其权限。
	const update = (member: Member, change: { roles?: string[]; group_id?: string; team_roles?: string[] }) =>
		act.mutate(() => updateMember(member.user_id, change));

	return (
		<div className="flex min-h-0 flex-col gap-4" data-testid="members-page">
			<header className="flex shrink-0 items-center justify-between gap-2">
				<div className="flex min-w-0 items-center gap-2">
					<SidebarTrigger className="shrink-0 md:hidden" />
					<h1 className="truncate text-lg font-semibold tracking-tight">{t("ml.members.title")}</h1>
				</div>
				<Button size="sm" onClick={() => setCreateOpen(true)} data-testid="member-create-button">
					<UserPlus />
					{t("ml.members.create")}
				</Button>
			</header>

			<Card>
				<CardHeader>
					<CardTitle className="text-sm">{t("ml.members.groups")}</CardTitle>
				</CardHeader>
				<CardContent className="flex flex-col gap-3">
					<form
						className="flex gap-2"
						onSubmit={(event) => {
							event.preventDefault();
							const name = groupName.trim();
							if (!name) return;
							act.mutate(() => createGroup(name).then(() => setGroupName("")));
						}}
					>
						<Input
							value={groupName}
							onChange={(event) => setGroupName(event.target.value)}
							placeholder={t("ml.members.groupName")}
							maxLength={60}
						/>
						<Button type="submit" size="sm" variant="outline">
							<Plus />
							{t("ml.members.addGroup")}
						</Button>
					</form>
					{groups.isError ? (
						<Alert variant="destructive">
							<AlertDescription>{problemText(groups.error, t("ml.members.failed"))}</AlertDescription>
						</Alert>
					) : (
						<div className="flex flex-wrap gap-2">
							{groupOptions.map((group) => (
								<div key={group.id} className="flex items-center gap-1 rounded border px-2 py-1 text-xs">
									<button
										type="button"
										className="hover:underline"
										onClick={() => {
											const name = window.prompt(t("ml.members.rename"), group.name)?.trim();
											if (name && name !== group.name) act.mutate(() => renameGroup(group.id, name));
										}}
									>
										{group.name}
									</button>
									<span className="text-muted-foreground">{t("ml.members.count", { n: group.members ?? 0 })}</span>
									<Button
										size="icon"
										variant="ghost"
										className="size-6"
										aria-label={t("ml.members.deleteGroup")}
										onClick={() => act.mutate(() => deleteGroup(group.id))}
									>
										<Trash2 />
									</Button>
								</div>
							))}
						</div>
					)}
				</CardContent>
			</Card>

			<Card>
				<CardHeader>
					<CardTitle className="text-sm">{t("ml.members.list")}</CardTitle>
				</CardHeader>
				<CardContent>
					{members.isError ? (
						<Alert variant="destructive">
							<AlertDescription>{problemText(members.error, t("ml.members.failed"))}</AlertDescription>
						</Alert>
					) : (
						<Table data-testid="members-table">
							<TableHeader>
								<TableRow>
									<TableHead>{t("ml.members.name")}</TableHead>
									<TableHead>{t("ml.members.role")}</TableHead>
									<TableHead>{t("ml.members.group")}</TableHead>
									<TableHead>{t("ml.members.teamRoles")}</TableHead>
									<TableHead className="w-16 text-right">{t("ml.actions")}</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{(members.data ?? []).map((member) => (
									<TableRow key={member.user_id}>
										<TableCell>
											<div className="font-medium">{member.display_name}</div>
											<div className="text-muted-foreground text-xs">{member.username || member.email}</div>
										</TableCell>
										<TableCell>
											<RolePicker
												roles={workspaceRoles}
												value={member.roles}
												onChange={(roles) => update(member, { roles })}
												placeholder={t("ml.members.pickRoles")}
											/>
										</TableCell>
										<TableCell>{groupSelect(member.group_id, (group_id) => update(member, { group_id }))}</TableCell>
										<TableCell>
											<RolePicker
												roles={teamRoles}
												value={member.team_roles}
												onChange={(team_roles) => update(member, { team_roles })}
												placeholder={t("ml.members.noTeamRoles")}
												disabled={!member.group_id}
											/>
										</TableCell>
										<TableCell className="text-right">
											<Button
												size="icon"
												variant="ghost"
												aria-label={t("ml.members.remove")}
												onClick={() => {
													if (window.confirm(t("ml.members.removeConfirm", { name: member.display_name })))
														act.mutate(() => removeMember(member.user_id));
												}}
											>
												<Trash2 />
											</Button>
										</TableCell>
									</TableRow>
								))}
							</TableBody>
						</Table>
					)}
				</CardContent>
			</Card>

			<Dialog open={createOpen} onOpenChange={setCreateOpen}>
				<DialogContent>
					<DialogHeader>
						<DialogTitle>{t("ml.members.create")}</DialogTitle>
						<DialogDescription>{t("ml.members.createHint")}</DialogDescription>
					</DialogHeader>
					<form className="flex flex-col gap-3" onSubmit={(event) => void submitMember(event)} id="member-create-form">
						<Label className="flex flex-col items-start gap-1">
							{t("ml.members.username")}
							<Input
								value={draft.username}
								onChange={(event) => setDraft({ ...draft, username: event.target.value })}
								pattern="[a-z0-9._-]{2,63}"
								required
							/>
						</Label>
						<Label className="flex flex-col items-start gap-1">
							{t("ml.members.name")}
							<Input
								value={draft.display_name}
								onChange={(event) => setDraft({ ...draft, display_name: event.target.value })}
								maxLength={80}
								required
							/>
						</Label>
						<Label className="flex flex-col items-start gap-1">
							{t("ml.members.email")}
							<Input type="email" value={draft.email} onChange={(event) => setDraft({ ...draft, email: event.target.value })} />
						</Label>
						<div className="flex flex-wrap items-center gap-3">
							<RolePicker
								roles={workspaceRoles}
								value={draft.roles}
								onChange={(roles) => setDraft({ ...draft, roles })}
								placeholder={t("ml.members.pickRoles")}
								testId="member-create-roles"
							/>
							{groupSelect(draft.group_id ?? "", (group_id) => setDraft({ ...draft, group_id, team_roles: [] }))}
							<RolePicker
								roles={teamRoles}
								value={draft.team_roles}
								onChange={(team_roles) => setDraft({ ...draft, team_roles })}
								placeholder={t("ml.members.noTeamRoles")}
								disabled={!draft.group_id}
								testId="member-create-team-roles"
							/>
						</div>
					</form>
					<DialogFooter>
						<Button type="submit" form="member-create-form">
							{t("ml.members.create")}
						</Button>
					</DialogFooter>
				</DialogContent>
			</Dialog>

			<Dialog open={created !== null} onOpenChange={(open) => !open && setCreated(null)}>
				<DialogContent>
					<DialogHeader>
						<DialogTitle>{t("ml.members.createdTitle")}</DialogTitle>
						<DialogDescription>{t("ml.members.createdHint")}</DialogDescription>
					</DialogHeader>
					{created && (
						<div className="flex flex-col gap-1 text-sm">
							<span>
								{t("ml.members.username")}：<code>{created.username}</code>
							</span>
							<span>
								{t("ml.members.oneTimePassword")}：<code data-testid="one-time-password">{created.one_time_password}</code>
							</span>
						</div>
					)}
					<DialogFooter>
						<Button onClick={() => setCreated(null)}>{t("ml.close")}</Button>
					</DialogFooter>
				</DialogContent>
			</Dialog>
		</div>
	);
}
