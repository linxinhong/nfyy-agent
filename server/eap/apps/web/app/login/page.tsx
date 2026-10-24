import { useEffect, useState, type FormEvent } from "react";
import { getRouteApi, useNavigate } from "@tanstack/react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useI18n } from "@/components/i18nProvider";
import { eapRequest, EapProblem } from "@/lib/api/eap";
import { changeLocalPassword } from "@/lib/api/mindlink";
import { login, loginWithPassword, PasswordChangeRequired } from "@/lib/auth/session";
import { ShieldCheck } from "lucide-react";

/**
 * 登录页。
 *
 * 内置账密登录（TASK-027）：网关签发会话 JWT，身份锚定 local_credentials
 * （bcrypt），角色仍来自 workspace_members。dev-login 表单仅在 dev 模式
 * 提供，作为 IdP 替身。两种方式共享同一套会话建立逻辑（lib/auth/session）。
 *
 * 登录成功后的会话 token 只存在内存 —— 这里不写任何浏览器存储。
 */
const route = getRouteApi("/login");

/** 只接受站内 Portal 路径作为登录后的去处，避免开放重定向。 */
function safeGoto(goto: string | undefined): string {
	return goto && goto.startsWith("/portal/") && !goto.startsWith("//") ? goto : "/portal/overview";
}

export function LoginPage() {
	const { t } = useI18n();
	const navigate = useNavigate();
	const { goto } = route.useSearch();
	const [mustChange, setMustChange] = useState(false);
	const [newPassword, setNewPassword] = useState("");
	const done = () => void navigate({ href: safeGoto(goto) });
	const [email, setEmail] = useState("");
	const [displayName, setDisplayName] = useState("");
	const [workspaces, setWorkspaces] = useState("secops");
	const [error, setError] = useState<{ code: string; message: string; traceId?: string } | null>(null);
	const [submitting, setSubmitting] = useState(false);
	const [localUsername, setLocalUsername] = useState("");
	const [localPassword, setLocalPassword] = useState("");
	const [showCurrentPassword, setShowCurrentPassword] = useState(false);
	const [showNewPassword, setShowNewPassword] = useState(false);
	const [localError, setLocalError] = useState<{ code: string; message: string; traceId?: string } | null>(null);
	const [localSubmitting, setLocalSubmitting] = useState(false);
	// 网关实际启用的登录方式（公开能力探测），决定渲染哪个表单。
	const [methods, setMethods] = useState<{ local: boolean; dev: boolean } | null>(null);
	const showLocal = methods?.local === true;
	const showDev = methods?.dev === true && !showLocal; // 账密可用时为首选，dev 表单让位

	useEffect(() => {
		let cancelled = false;
		eapRequest<{ local: boolean; dev: boolean }>("/enterprise/v1/auth/methods")
			.then((m) => {
				if (!cancelled) setMethods(m);
			})
			.catch(() => {
				if (!cancelled) setMethods({ local: false, dev: true });
			});
		return () => {
			cancelled = true;
		};
	}, []);

	const finish = (run: () => Promise<unknown>) => {
		setError(null);
		setLocalError(null);
		setSubmitting(true);
		run()
			.then(done)
			.catch((cause: unknown) => {
				if (cause instanceof EapProblem) {
					setError({ code: cause.code, message: cause.message, traceId: cause.traceId });
				} else {
					setError({ code: "unknown", message: String(cause) });
				}
			})
			.finally(() => setSubmitting(false));
	};

	const submit = (event: FormEvent) => {
		event.preventDefault();
		const workspaceIds = workspaces
			.split(/[,\s]+/)
			.map((id) => id.trim())
			.filter(Boolean);
		finish(() => login(email.trim(), displayName.trim(), workspaceIds));
	};

	const submitLocal = (event: FormEvent) => {
		event.preventDefault();
		setLocalError(null);
		const form = event.currentTarget as HTMLFormElement;
		const currentPassword = form.querySelector<HTMLInputElement>("#local-password")?.value ?? localPassword;
		const nextPassword = form.querySelector<HTMLInputElement>("#local-new-password")?.value ?? newPassword;
		if (mustChange) {
			if (nextPassword.length < 8) {
				setLocalError({ code: "invalid_argument", message: t("eap.login.newPasswordTooShort") });
				return;
			}
			if (nextPassword === currentPassword) {
				setLocalError({ code: "invalid_argument", message: t("eap.login.newPasswordMatchesCurrent") });
				return;
			}
		}
		setLocalSubmitting(true);
		const username = localUsername.trim();
		const run = mustChange
			? changeLocalPassword(username, currentPassword, nextPassword).then(() => loginWithPassword(username, nextPassword))
			: loginWithPassword(username, currentPassword);
		run
			.then(done)
			.catch((cause: unknown) => {
				if (cause instanceof PasswordChangeRequired) {
					setMustChange(true);
					setNewPassword("");
					setLocalError(null);
				} else if (cause instanceof EapProblem) {
					setLocalError({
						code: cause.code,
						message:
							cause.code === "unauthenticated"
								? t("eap.login.invalidCredentials")
								: mustChange && cause.code === "invalid_argument"
									? t("eap.login.newPasswordRejected")
									: cause.message,
						traceId: cause.traceId,
					});
				} else {
					setLocalError({ code: "unknown", message: String(cause) });
				}
			})
			.finally(() => setLocalSubmitting(false));
	};

	return (
		<div className="flex h-full flex-col items-center justify-center gap-6 px-4">
			<div className="flex flex-col items-center gap-2 text-center">
				<ShieldCheck className="text-primary size-8" aria-hidden="true" />
				<h1 className="text-lg font-semibold tracking-tight">知识服务中心</h1>
				<p className="text-muted-foreground max-w-sm text-xs">{t("eap.login.hint")}</p>
			</div>

			{methods === null ? (
				<p className="text-muted-foreground text-xs">{t("eap.loading")}</p>
			) : !showLocal && !showDev ? (
				<Alert variant="destructive" className="w-full max-w-sm" data-testid="login-unavailable">
					<AlertDescription>{t("eap.login.unavailable")}</AlertDescription>
				</Alert>
			) : null}

			{showLocal ? (
				<Card className="w-full max-w-sm" data-testid="local-login-card">
					<CardHeader>
						<CardTitle className="flex items-center justify-between gap-2 text-sm">
							{t("eap.login.localTitle")}
							<Badge variant="outline">username / password</Badge>
						</CardTitle>
					</CardHeader>
					<CardContent>
						<form onSubmit={submitLocal} className="flex flex-col gap-3">
							<div className="flex flex-col gap-1.5">
								<Label htmlFor="local-username">{t("eap.login.localUsername")}</Label>
								<Input
									id="local-username"
									data-testid="local-username"
									name="username"
									autoComplete="username"
									required
									value={localUsername}
									onChange={(event) => {
										setLocalUsername(event.target.value);
										setLocalError(null);
									}}
									placeholder="lin"
									autoFocus
								/>
							</div>
							<div className="flex flex-col gap-1.5">
								<div className="flex items-center justify-between">
									<Label htmlFor="local-password">{t("eap.login.localPassword")}</Label>
									<button
										type="button"
										className="text-muted-foreground hover:text-foreground text-xs"
										onClick={() => setShowCurrentPassword((shown) => !shown)}
									>
										{t(showCurrentPassword ? "eap.login.hidePassword" : "eap.login.showPassword")}
									</button>
								</div>
								<Input
									id="local-password"
									data-testid="local-password"
									name="current_password"
									type={showCurrentPassword ? "text" : "password"}
									autoComplete="current-password"
									required
									value={localPassword}
									onChange={(event) => {
										setLocalPassword(event.target.value);
										setLocalError(null);
									}}
								/>
							</div>
							{mustChange ? (
								<div className="flex flex-col gap-1.5">
									<div className="flex items-center justify-between">
										<Label htmlFor="local-new-password">{t("ml.login.newPassword")}</Label>
										<button
											type="button"
											className="text-muted-foreground hover:text-foreground text-xs"
											onClick={() => setShowNewPassword((shown) => !shown)}
										>
											{t(showNewPassword ? "eap.login.hidePassword" : "eap.login.showPassword")}
										</button>
									</div>
									<Input
										id="local-new-password"
										data-testid="local-new-password"
										name="new_password"
										type={showNewPassword ? "text" : "password"}
										autoComplete="new-password"
										required
										minLength={8}
										value={newPassword}
										onChange={(event) => {
											setNewPassword(event.target.value);
											setLocalError(null);
										}}
									/>
									<p className="text-muted-foreground text-xs">{t("ml.login.mustChange")}</p>
								</div>
							) : null}

							{localError ? (
								<Alert variant="destructive" data-testid="local-login-error">
									<AlertDescription>
										<p className="text-sm font-medium">{localError.message}</p>
										<p className="text-muted-foreground text-xs">
											{t("eap.error.code")}: {localError.code}
											{localError.traceId ? ` · ${t("eap.error.trace")}: ${localError.traceId}` : ""}
										</p>
									</AlertDescription>
								</Alert>
							) : null}

							<Button type="submit" data-testid="local-login-submit" isLoading={localSubmitting} disabled={localSubmitting}>
								{t("eap.login.localSubmit")}
							</Button>
						</form>
					</CardContent>
				</Card>
			) : null}

			{showDev ? (
				<form onSubmit={submit} className="flex w-full max-w-sm flex-col gap-3" data-testid="login-form">
					<div className="flex flex-col gap-1.5">
						<Label htmlFor="login-email">{t("eap.login.email")}</Label>
						<Input
							id="login-email"
							data-testid="login-email"
							type="email"
							required
							value={email}
							onChange={(event) => setEmail(event.target.value)}
							placeholder="li.lei@example.com"
							autoFocus
						/>
					</div>
					<div className="flex flex-col gap-1.5">
						<Label htmlFor="login-name">{t("eap.login.displayName")}</Label>
						<Input
							id="login-name"
							data-testid="login-name"
							value={displayName}
							onChange={(event) => setDisplayName(event.target.value)}
							placeholder="Li Lei"
						/>
					</div>
					<div className="flex flex-col gap-1.5">
						<Label htmlFor="login-workspaces">{t("eap.login.workspaces")}</Label>
						<Input
							id="login-workspaces"
							data-testid="login-workspaces"
							value={workspaces}
							onChange={(event) => setWorkspaces(event.target.value)}
							placeholder="secops"
						/>
						<p className="text-muted-foreground text-xs">{t("eap.login.workspacesHint")}</p>
					</div>

					{error ? (
						<Alert variant="destructive" data-testid="login-error">
							<AlertDescription>
								<p className="text-sm font-medium">{error.message}</p>
								<p className="text-muted-foreground text-xs">
									{t("eap.error.code")}: {error.code}
									{error.traceId ? ` · ${t("eap.error.trace")}: ${error.traceId}` : ""}
								</p>
							</AlertDescription>
						</Alert>
					) : null}

					<Button type="submit" data-testid="login-submit" isLoading={submitting} disabled={submitting}>
						{t("eap.login.submit")}
					</Button>
				</form>
			) : null}
		</div>
	);
}
