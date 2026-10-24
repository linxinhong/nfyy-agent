import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useI18n } from "@/components/i18nProvider";
import { deleteRole, listRoles, problemText, putRole, type Permission, type Role } from "@/lib/api/mindlink";
import { usePermissions } from "@/lib/auth/permissions";
import { useSession } from "@/lib/auth/session";
import { toast } from "sonner";
import { Plus } from "lucide-react";

type Draft = { key: string; name: string; scope: Role["scope"]; permissions: Permission[]; existing: boolean };

/**
 * 角色与权限 —— 查看角色目录，新建或修改角色。
 *
 * 能看（管理成员）和能改（管理角色）是两项权限；没有“管理角色”时只读。
 * 一个角色只能包含编辑者自己拥有的权限（网关校验），团队角色只能勾选可在
 * 团队内授予的权限。内置角色可以改名和改权限，也可恢复默认；管理员角色锁定。
 */
export function RolesPage() {
	const { t } = useI18n();
	const session = useSession();
	const queryClient = useQueryClient();
	const workspace = session?.selectedWorkspace ?? "";
	const canEdit = usePermissions().has("roles.manage");
	const catalog = useQuery({ queryKey: ["mindlink", "roles", workspace], queryFn: listRoles });
	const [draft, setDraft] = useState<Draft | null>(null);
	const act = useMutation({
		mutationFn: (run: () => Promise<unknown>) => run(),
		onSuccess: () => {
			toast.success(t("ml.saved"));
			setDraft(null);
			void queryClient.invalidateQueries({ queryKey: ["mindlink", "roles"] });
			void queryClient.invalidateQueries({ queryKey: ["mindlink", "permissions"] });
		},
		onError: (error) => toast.error(problemText(error, t("ml.roles.failed"))),
	});

	const permissions = catalog.data?.permissions ?? [];
	const permName = (key: Permission) => t(`ml.perm.${key}` as "ml.perm.roles.manage");
	const grantable = (scope: Role["scope"]) => permissions.filter((p) => scope === "workspace" || p.team_grantable);

	return (
		<div className="flex min-h-0 flex-col gap-4" data-testid="roles-page">
			<header className="flex shrink-0 items-center justify-between gap-2">
				<div className="flex min-w-0 items-center gap-2">
					<SidebarTrigger className="shrink-0 md:hidden" />
					<h1 className="truncate text-lg font-semibold tracking-tight">{t("ml.roles.title")}</h1>
				</div>
				{canEdit && (
					<Button
						size="sm"
						onClick={() => setDraft({ key: "", name: "", scope: "workspace", permissions: [], existing: false })}
						data-testid="role-create-button"
					>
						<Plus />
						{t("ml.roles.create")}
					</Button>
				)}
			</header>
			<p className="text-muted-foreground text-sm">{t("ml.roles.hint")}</p>
			{!canEdit && <p className="text-muted-foreground text-xs">{t("ml.roles.readonly")}</p>}

			<Card>
				<CardContent>
					{catalog.isError ? (
						<Alert variant="destructive">
							<AlertDescription>{problemText(catalog.error, t("ml.roles.failed"))}</AlertDescription>
						</Alert>
					) : (
						<Table data-testid="roles-table">
							<TableHeader>
								<TableRow>
									<TableHead>{t("ml.roles.name")}</TableHead>
									<TableHead>{t("ml.roles.scope")}</TableHead>
									<TableHead>{t("ml.roles.permissions")}</TableHead>
									<TableHead className="w-40 text-right">{t("ml.actions")}</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{(catalog.data?.roles ?? []).map((role) => (
									<TableRow key={role.key}>
										<TableCell>
											<div className="font-medium">
												{role.name} {role.builtin && <Badge variant="secondary">{t("ml.roles.builtin")}</Badge>}
											</div>
											<div className="text-muted-foreground text-xs">{role.key}</div>
										</TableCell>
										<TableCell>{t(`ml.roles.scope.${role.scope}`)}</TableCell>
										<TableCell>
											<div className="flex flex-wrap gap-1">
												{role.permissions.length === 0 ? (
													<span className="text-muted-foreground text-xs">{t("ml.roles.none")}</span>
												) : (
													role.permissions.map((p) => (
														<Badge key={p} variant="outline">
															{permName(p)}
														</Badge>
													))
												)}
											</div>
										</TableCell>
										<TableCell className="text-right">
											{canEdit && !role.locked && (
												<div className="flex justify-end gap-1">
													<Button
														size="sm"
														variant="ghost"
														onClick={() => setDraft({ ...role, existing: true })}
														data-testid={`role-edit-${role.key}`}
													>
														{t("ml.roles.edit")}
													</Button>
													<Button
														size="sm"
														variant="ghost"
														onClick={() => {
															const message = role.builtin ? "ml.roles.resetConfirm" : "ml.roles.deleteConfirm";
															if (window.confirm(t(message, { name: role.name }))) act.mutate(() => deleteRole(role.key));
														}}
													>
														{role.builtin ? t("ml.roles.reset") : t("ml.roles.delete")}
													</Button>
												</div>
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
				<DialogContent>
					<DialogHeader>
						<DialogTitle>{draft?.existing ? t("ml.roles.edit") : t("ml.roles.create")}</DialogTitle>
						<DialogDescription>{draft?.scope === "team" ? t("ml.roles.teamOnly") : t("ml.roles.hint")}</DialogDescription>
					</DialogHeader>
					{draft && (
						<form
							id="role-form"
							className="flex flex-col gap-3"
							onSubmit={(event) => {
								event.preventDefault();
								const { key, name, scope, permissions: perms } = draft;
								act.mutate(() => putRole(key.trim(), { name: name.trim(), scope, permissions: perms }));
							}}
						>
							<Label className="flex flex-col items-start gap-1">
								{t("ml.roles.key")}
								<Input
									value={draft.key}
									onChange={(event) => setDraft({ ...draft, key: event.target.value })}
									pattern="[a-z][a-z0-9_\-]{0,31}"
									disabled={draft.existing}
									required
								/>
								<span className="text-muted-foreground text-xs font-normal">{t("ml.roles.keyHint")}</span>
							</Label>
							<Label className="flex flex-col items-start gap-1">
								{t("ml.roles.name")}
								<Input value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} maxLength={40} required />
							</Label>
							<Label className="flex flex-col items-start gap-1">
								{t("ml.roles.scope")}
								<Select
									value={draft.scope}
									disabled={draft.existing}
									onValueChange={(scope) => {
										const next = scope as Role["scope"];
										const allowed = grantable(next).map((p) => p.key);
										setDraft({ ...draft, scope: next, permissions: draft.permissions.filter((p) => allowed.includes(p)) });
									}}
								>
									<SelectTrigger className="h-8 w-36">
										<SelectValue />
									</SelectTrigger>
									<SelectContent>
										<SelectItem value="workspace">{t("ml.roles.scope.workspace")}</SelectItem>
										<SelectItem value="team">{t("ml.roles.scope.team")}</SelectItem>
									</SelectContent>
								</Select>
							</Label>
							<fieldset className="flex flex-col gap-2">
								<legend className="mb-1 text-sm font-medium">{t("ml.roles.permissions")}</legend>
								{grantable(draft.scope).map((p) => (
									<Label key={p.key} className="flex items-start gap-2 font-normal">
										<Checkbox
											className="mt-0.5"
											checked={draft.permissions.includes(p.key)}
											onCheckedChange={(checked) =>
												setDraft({
													...draft,
													permissions: checked === true ? [...draft.permissions, p.key] : draft.permissions.filter((k) => k !== p.key),
												})
											}
										/>
										<span className="flex flex-col">
											{permName(p.key)}
											<span className="text-muted-foreground text-xs">{t(`ml.perm.${p.key}.hint` as "ml.perm.roles.manage.hint")}</span>
										</span>
									</Label>
								))}
							</fieldset>
						</form>
					)}
					<DialogFooter>
						<Button type="submit" form="role-form" disabled={act.isPending}>
							{t("ml.save")}
						</Button>
					</DialogFooter>
				</DialogContent>
			</Dialog>
		</div>
	);
}
