import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useI18n } from "@/components/i18nProvider";
import { EapProblem, createKey, listKeys, revokeKey, type APIKeyCreated, type APIKeyView } from "@/lib/api/eap";
import { useSession } from "@/lib/auth/session";
import { buildClientConfig, downloadClientConfig } from "@/lib/utils/clientConfig";
import { toast } from "sonner";
import { Download, KeyRound, Plus, Trash2 } from "lucide-react";

const keysQueryKey = ["eap", "keys"] as const;

/**
 * 我的 API Key —— 生命周期管理。
 *
 * 一次性展示规则（PRD 6.3）：完整 key 值只出现在创建响应里，由
 * OneTimeKeyDialog 持有在组件状态中，关闭即消失；列表只显示前缀。
 * 任何 key 值都不写浏览器存储。
 */
export function KeysPage() {
	const { t } = useI18n();
	const session = useSession();
	const queryClient = useQueryClient();
	const [createOpen, setCreateOpen] = useState(false);
	const [oneTime, setOneTime] = useState<APIKeyCreated | null>(null);
	const [revokeTarget, setRevokeTarget] = useState<APIKeyView | null>(null);

	const keysQuery = useQuery({
		queryKey: keysQueryKey,
		queryFn: listKeys,
	});

	const revokeMutation = useMutation({
		mutationFn: (keyId: string) => revokeKey(keyId),
		onSuccess: () => {
			toast.success(t("eap.keys.revoked"));
			setRevokeTarget(null);
			void queryClient.invalidateQueries({ queryKey: keysQueryKey });
		},
		onError: (error) => toast.error(describe(error, t)),
	});

	const statusOf = (key: APIKeyView): "revoked" | "expired" | "active" => {
		if (key.revoked_at) return "revoked";
		if (key.expires_at && new Date(key.expires_at).getTime() < Date.now()) return "expired";
		return "active";
	};

	return (
		<div className="flex min-h-0 flex-col gap-4" data-testid="keys-page">
			<header className="flex shrink-0 items-center justify-between gap-2">
				<div className="flex min-w-0 items-center gap-2">
					<SidebarTrigger className="shrink-0 md:hidden" />
					<h1 className="truncate text-lg font-semibold tracking-tight">{t("eap.keys.title")}</h1>
				</div>
				<Button size="sm" data-testid="key-create-button" onClick={() => setCreateOpen(true)}>
					<Plus />
					{t("eap.keys.create")}
				</Button>
			</header>

			<Card>
				<CardHeader>
					<CardTitle className="text-sm">{t("eap.keys.listTitle")}</CardTitle>
				</CardHeader>
				<CardContent>
					{keysQuery.isError ? (
						<Alert variant="destructive" data-testid="keys-error">
							<AlertDescription>{describe(keysQuery.error, t)}</AlertDescription>
						</Alert>
					) : keysQuery.isLoading ? (
						<p className="text-muted-foreground py-8 text-center text-xs">{t("eap.loading")}</p>
					) : !keysQuery.data || keysQuery.data.length === 0 ? (
						<p className="text-muted-foreground py-8 text-center text-xs">{t("eap.keys.empty")}</p>
					) : (
						<Table data-testid="keys-table">
							<TableHeader>
								<TableRow>
									<TableHead>{t("eap.keys.prefix")}</TableHead>
									<TableHead>{t("eap.keys.name")}</TableHead>
									<TableHead className="hidden md:table-cell">{t("eap.keys.createdAt")}</TableHead>
									<TableHead className="hidden lg:table-cell">{t("eap.keys.lastUsed")}</TableHead>
									<TableHead>{t("eap.keys.status")}</TableHead>
									<TableHead className="w-24 text-right">{t("eap.keys.actions")}</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{keysQuery.data.map((key) => {
									const status = statusOf(key);
									return (
										<TableRow key={key.key_id} data-testid="key-row">
											<TableCell className="font-mono text-xs">{key.prefix}…</TableCell>
											<TableCell className="max-w-40 truncate">{key.name}</TableCell>
											<TableCell className="hidden text-xs md:table-cell">{fmtDate(key.created_at)}</TableCell>
											<TableCell className="hidden text-xs lg:table-cell">{key.last_used_at ? fmtDate(key.last_used_at) : "—"}</TableCell>
											<TableCell>
												{status === "active" ? (
													<Badge data-testid={`key-status-${key.prefix}`} variant="success">
														{t("eap.keys.statusActive")}
													</Badge>
												) : status === "revoked" ? (
													<Badge data-testid={`key-status-${key.prefix}`} variant="destructive">
														{t("eap.keys.statusRevoked")}
													</Badge>
												) : (
													<Badge data-testid={`key-status-${key.prefix}`} variant="warning">
														{t("eap.keys.statusExpired")}
													</Badge>
												)}
											</TableCell>
											<TableCell className="text-right">
												<div className="flex justify-end gap-1">
													<Button
														variant="ghost"
														size="icon"
														className="size-7"
														title={t("eap.keys.downloadConfig")}
														data-testid={`key-config-${key.prefix}`}
														onClick={() => {
															downloadClientConfig(buildClientConfig(window.location.origin, session?.selectedWorkspace ?? ""));
															toast.success(t("eap.keys.configDownloaded"));
														}}
													>
														<Download className="size-3.5" />
													</Button>
													{status === "active" ? (
														<Button
															variant="ghost"
															size="icon"
															className="text-destructive size-7"
															title={t("eap.keys.revoke")}
															data-testid={`key-revoke-${key.prefix}`}
															onClick={() => setRevokeTarget(key)}
														>
															<Trash2 className="size-3.5" />
														</Button>
													) : null}
												</div>
											</TableCell>
										</TableRow>
									);
								})}
							</TableBody>
						</Table>
					)}
				</CardContent>
			</Card>

			<CreateKeyDialog
				open={createOpen}
				onOpenChange={setCreateOpen}
				onCreated={(created) => {
					setCreateOpen(false);
					setOneTime(created);
					void queryClient.invalidateQueries({ queryKey: keysQueryKey });
				}}
			/>

			<OneTimeKeyDialog created={oneTime} onClose={() => setOneTime(null)} />

			<Dialog open={revokeTarget !== null} onOpenChange={(open) => !open && setRevokeTarget(null)}>
				<DialogContent className="sm:max-w-sm" data-testid="revoke-dialog">
					<DialogHeader>
						<DialogTitle>{t("eap.keys.revokeConfirmTitle")}</DialogTitle>
						<DialogDescription>{t("eap.keys.revokeConfirmBody")}</DialogDescription>
					</DialogHeader>
					<DialogFooter>
						<Button variant="outline" size="sm" onClick={() => setRevokeTarget(null)}>
							{t("eap.cancel")}
						</Button>
						<Button
							variant="destructive"
							size="sm"
							data-testid="revoke-confirm"
							isLoading={revokeMutation.isPending}
							onClick={() => revokeTarget && revokeMutation.mutate(revokeTarget.key_id)}
						>
							{t("eap.keys.revoke")}
						</Button>
					</DialogFooter>
				</DialogContent>
			</Dialog>
		</div>
	);
}

function CreateKeyDialog({
	open,
	onOpenChange,
	onCreated,
}: {
	open: boolean;
	onOpenChange: (open: boolean) => void;
	onCreated: (created: APIKeyCreated) => void;
}) {
	const { t } = useI18n();
	const [name, setName] = useState("");
	const [environment, setEnvironment] = useState<"live" | "test">("live");
	const [error, setError] = useState<string | null>(null);

	const createMutation = useMutation({
		mutationFn: () => createKey({ name: name.trim(), environment }),
		onSuccess: (created) => {
			setName("");
			setError(null);
			onCreated(created);
		},
		onError: (cause) => setError(describe(cause, t)),
	});

	const submit = (event: FormEvent) => {
		event.preventDefault();
		if (name.trim()) createMutation.mutate();
	};

	return (
		<Dialog open={open} onOpenChange={onOpenChange}>
			<DialogContent className="sm:max-w-sm" data-testid="create-key-dialog">
				<DialogHeader>
					<DialogTitle>{t("eap.keys.create")}</DialogTitle>
					<DialogDescription>{t("eap.keys.createHint")}</DialogDescription>
				</DialogHeader>
				<form onSubmit={submit} className="flex flex-col gap-3">
					<div className="flex flex-col gap-1.5">
						<Label htmlFor="key-name">{t("eap.keys.name")}</Label>
						<Input
							id="key-name"
							data-testid="key-name-input"
							required
							value={name}
							onChange={(event) => setName(event.target.value)}
							placeholder="MacBook Harness"
						/>
					</div>
					<div className="flex flex-col gap-1.5">
						<Label>{t("eap.keys.environment")}</Label>
						<Select value={environment} onValueChange={(value) => setEnvironment(value as "live" | "test")}>
							<SelectTrigger data-testid="key-environment" size="sm">
								<SelectValue />
							</SelectTrigger>
							<SelectContent>
								<SelectItem value="live">live（agk_live_…）</SelectItem>
								<SelectItem value="test">test（agk_test_…）</SelectItem>
							</SelectContent>
						</Select>
					</div>
					{error ? (
						<Alert variant="destructive">
							<AlertDescription className="text-xs">{error}</AlertDescription>
						</Alert>
					) : null}
					<DialogFooter>
						<Button type="button" variant="outline" size="sm" onClick={() => onOpenChange(false)}>
							{t("eap.cancel")}
						</Button>
						<Button type="submit" size="sm" data-testid="key-submit" isLoading={createMutation.isPending} disabled={!name.trim()}>
							<KeyRound />
							{t("eap.keys.create")}
						</Button>
					</DialogFooter>
				</form>
			</DialogContent>
		</Dialog>
	);
}

/**
 * 一次性展示对话框 —— key 值只活在组件状态里。
 * 关闭（或刷新页面）后无法再取回；列表与后端都只有哈希与前缀。
 */
function OneTimeKeyDialog({ created, onClose }: { created: APIKeyCreated | null; onClose: () => void }) {
	const { t } = useI18n();
	const session = useSession();
	if (!created) return null;
	return (
		<Dialog open onOpenChange={(open) => !open && onClose()}>
			<DialogContent className="sm:max-w-md" data-testid="one-time-key-dialog">
				<DialogHeader>
					<DialogTitle>{t("eap.keys.oneTimeTitle")}</DialogTitle>
					<DialogDescription>{t("eap.keys.oneTimeWarning")}</DialogDescription>
				</DialogHeader>
				<div className="flex flex-col gap-3">
					<code className="bg-muted overflow-x-auto rounded-sm p-3 font-mono text-xs break-all" data-testid="one-time-key-value">
						{created.key}
					</code>
					<div className="flex flex-wrap gap-2">
						<Button
							variant="outline"
							size="sm"
							data-testid="copy-key"
							onClick={() => {
								void navigator.clipboard.writeText(created.key);
								toast.success(t("eap.keys.copied"));
							}}
						>
							{t("eap.keys.copy")}
						</Button>
						<Button
							variant="outline"
							size="sm"
							data-testid="download-config-from-key"
							onClick={() => {
								downloadClientConfig(buildClientConfig(window.location.origin, session?.selectedWorkspace ?? ""));
								toast.success(t("eap.keys.configDownloaded"));
							}}
						>
							<Download />
							{t("eap.keys.downloadConfig")}
						</Button>
					</div>
					<p className="text-muted-foreground text-xs">{t("eap.keys.oneTimeHint")}</p>
				</div>
				<DialogFooter>
					<Button size="sm" data-testid="one-time-key-close" onClick={onClose}>
						{t("eap.keys.saved")}
					</Button>
				</DialogFooter>
			</DialogContent>
		</Dialog>
	);
}

// 组件外工具函数。

function fmtDate(value: string): string {
	return new Date(value).toLocaleString();
}

function describe(error: unknown, t: ReturnType<typeof useI18n>["t"]): string {
	if (error instanceof EapProblem) {
		const trace = error.traceId ? ` · ${t("eap.error.trace")}: ${error.traceId}` : "";
		return `${error.message}（${t("eap.error.code")}: ${error.code}${trace}）`;
	}
	return String(error);
}
