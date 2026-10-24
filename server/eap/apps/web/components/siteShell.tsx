import { Link, useLocation, useNavigate } from "@tanstack/react-router";
import { BookOpen, ChevronDown, LogOut, RefreshCw, Settings2, UserRound } from "lucide-react";
import type { ReactNode } from "react";
import { useI18n } from "@/components/i18nProvider";
import { ThemeToggle } from "@/components/themeToggle";
import { Button } from "@/components/ui/button";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuSeparator,
	DropdownMenuTrigger,
} from "@/components/ui/dropdownMenu";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { logout, setSelectedWorkspace, useSession } from "@/lib/auth/session";
import { usePermissions } from "@/lib/auth/permissions";
import { desktopBridgeToken } from "@/lib/auth/desktopPortal";
import { toast } from "sonner";

const sections = [
	{ to: "/portal/overview", key: "eap.overview.title" },
	{ to: "/portal/news", key: "site.nav.news" },
	{ to: "/portal/wiki", key: "site.nav.wiki" },
	{ to: "/portal/catalog", key: "site.nav.catalog" },
] as const;

/** Member-facing site chrome. Workspace and management remain reachable without dominating the page. */
export function SiteShell({ children }: { children: ReactNode }) {
	const { t } = useI18n();
	const session = useSession();
	const { has, hasAny } = usePermissions();
	const navigate = useNavigate();
	const pathname = useLocation({ select: (location) => location.pathname });
	const desktopKeySession = session?.token === desktopBridgeToken;
	const signOut = async () => {
		if (desktopKeySession) {
			try {
				await window.yuanpuPortal?.clearAPIKey();
			} catch {
				toast.error(t("site.logoutFailed"));
				return;
			}
		}
		logout();
		void navigate({ to: "/login" });
	};

	return (
		<div className="bg-background min-h-dvh">
			<header className="bg-background/95 sticky top-0 z-30 border-b backdrop-blur">
				<div className="mx-auto flex min-h-12 w-full flex-wrap items-center gap-x-5 gap-y-1 px-4 py-1 sm:px-5 lg:px-6">
					<Link to="/portal/overview" className="flex shrink-0 items-center gap-2.5 font-semibold tracking-tight">
						<span className="bg-primary text-primary-foreground flex size-8 items-center justify-center rounded-lg">
							<BookOpen className="size-4" />
						</span>
						<span>知识服务中心</span>
					</Link>
					<nav
						aria-label={t("site.navigation")}
						className="order-3 flex w-full items-center gap-1 overflow-x-auto sm:order-none sm:w-auto sm:flex-1"
					>
						{sections.map((section) => (
							<Link
								key={section.to}
								to={section.to}
								className={`shrink-0 border-b-2 px-3 py-1.5 text-sm transition-colors ${pathname === section.to ? "border-primary text-foreground font-medium" : "text-muted-foreground hover:text-foreground border-transparent"}`}
							>
								{t(section.key)}
							</Link>
						))}
					</nav>
					<div className="ml-auto flex items-center gap-2">
						{session && session.workspaces.length > 0 && (
							<Select value={session.selectedWorkspace} onValueChange={setSelectedWorkspace}>
								<SelectTrigger
									size="sm"
									className="w-28 sm:w-40"
									aria-label={t("eap.overview.selectedWorkspace")}
									data-testid="workspace-select"
								>
									<SelectValue />
								</SelectTrigger>
								<SelectContent>
									{session.workspaces.map((workspace) => (
										<SelectItem key={workspace.id} value={workspace.id}>
											{workspace.name}
										</SelectItem>
									))}
								</SelectContent>
							</Select>
						)}
						<Button variant="ghost" size="icon" onClick={() => window.location.reload()} aria-label="刷新页面" title="刷新页面">
							<RefreshCw className="size-4" />
						</Button>
						<ThemeToggle />
						<DropdownMenu>
							<DropdownMenuTrigger asChild>
								<Button variant="ghost" size="sm" className="max-w-40 gap-1" data-testid="site-account-menu">
									<UserRound className="size-4" />
									<span className="hidden truncate sm:inline">{session?.user.display_name || session?.user.email}</span>
									<ChevronDown className="size-3" />
								</Button>
							</DropdownMenuTrigger>
							<DropdownMenuContent align="end" className="w-56">
								<div className="text-muted-foreground px-2 py-1.5 text-xs" data-testid="profile-email">
									{session?.user.email}
								</div>
								<DropdownMenuSeparator />
								{!desktopKeySession && (
									<DropdownMenuItem asChild>
										<Link to="/portal/keys" data-testid="quick-keys">
											{t("eap.nav.keys")}
										</Link>
									</DropdownMenuItem>
								)}
								{!desktopKeySession && (
									<DropdownMenuItem asChild>
										<Link to="/portal/agent-center" data-testid="quick-agent-center">
											{t("eap.nav.agentCenter")}
										</Link>
									</DropdownMenuItem>
								)}
								{!desktopKeySession && (hasAny("knowledge.review") || has("members.manage") || has("platform.admin")) && (
									<DropdownMenuItem asChild>
										<Link to="/portal/review">
											<Settings2 />
											{t("site.workspace")}
										</Link>
									</DropdownMenuItem>
								)}
								<DropdownMenuSeparator />
								<DropdownMenuItem onSelect={() => void signOut()} data-testid="logout">
									<LogOut />
									{t("eap.overview.logout")}
								</DropdownMenuItem>
							</DropdownMenuContent>
						</DropdownMenu>
					</div>
				</div>
			</header>
			<main className="mx-auto w-full px-4 py-5 sm:px-5 lg:px-6">{children}</main>
		</div>
	);
}
