import { useState } from "react";
import { getRouteApi } from "@tanstack/react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { useI18n } from "@/components/i18nProvider";
import { approveDevice, denyDevice, problemText } from "@/lib/api/mindlink";
import { useSession } from "@/lib/auth/session";
import { MonitorSmartphone } from "lucide-react";

const route = getRouteApi("/portal/device");

/**
 * 设备登录确认 —— 桌面端发起设备登录后在浏览器打开此页，已登录成员核对
 * 桌面上显示的验证码后确认或拒绝。验证码由用户核对，不能只凭链接自动批准。
 */
export function DevicePage() {
	const { t } = useI18n();
	const session = useSession();
	const search = route.useSearch();
	const [code, setCode] = useState(search.user_code ?? "");
	const [state, setState] = useState<"idle" | "approved" | "denied">("idle");
	const [error, setError] = useState("");
	const run = async (action: (code: string) => Promise<unknown>, next: "approved" | "denied") => {
		setError("");
		try {
			await action(code.trim());
			setState(next);
		} catch (cause) {
			setError(problemText(cause, t("ml.device.failed")));
		}
	};
	return (
		<div className="flex justify-center py-8" data-testid="device-page">
			<Card className="w-full max-w-md">
				<CardHeader>
					<CardTitle className="flex items-center gap-2">
						<MonitorSmartphone className="size-5" />
						{t("ml.device.title")}
					</CardTitle>
				</CardHeader>
				<CardContent className="flex flex-col gap-3 text-sm">
					{state === "approved" ? (
						<Alert>
							<AlertDescription>{t("ml.device.approved")}</AlertDescription>
						</Alert>
					) : state === "denied" ? (
						<Alert>
							<AlertDescription>{t("ml.device.denied")}</AlertDescription>
						</Alert>
					) : (
						<>
							<p className="text-muted-foreground">
								{t("ml.device.hint", { name: session?.user.display_name || session?.user.email || "" })}
							</p>
							<Input
								value={code}
								onChange={(event) => setCode(event.target.value.toUpperCase())}
								placeholder={t("ml.device.code")}
								maxLength={16}
								data-testid="device-code"
							/>
							{error && (
								<Alert variant="destructive">
									<AlertDescription>{error}</AlertDescription>
								</Alert>
							)}
							<div className="flex gap-2">
								<Button disabled={!code.trim()} onClick={() => void run(approveDevice, "approved")}>
									{t("ml.device.approve")}
								</Button>
								<Button variant="outline" disabled={!code.trim()} onClick={() => void run(denyDevice, "denied")}>
									{t("ml.device.deny")}
								</Button>
							</div>
						</>
					)}
				</CardContent>
			</Card>
		</div>
	);
}
