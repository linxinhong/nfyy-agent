import { Check, Moon, Sun } from "lucide-react";
import { useTheme } from "next-themes";

import { Button } from "@/components/ui/button";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuLabel,
	DropdownMenuSeparator,
	DropdownMenuTrigger,
} from "@/components/ui/dropdownMenu";
import { PALETTES } from "@/lib/constants/palettes";
import { usePalette } from "@/lib/hooks/usePalette";
import { useI18n } from "@/components/i18nProvider";

/**
 * 外观菜单 —— 明暗 + 主题色，一个入口。
 *
 * 明暗走 next-themes（class），主题色走 usePalette（data-palette），二者正交。
 * 尺寸与侧栏导航项一致：32×32 触区、16px 图标。
 */
export function ThemeToggle() {
	const { t, locale, setLocale } = useI18n();
	const { setTheme } = useTheme();
	const { palette, setPalette } = usePalette();

	return (
		<DropdownMenu>
			<DropdownMenuTrigger asChild>
				<Button
					variant="ghost"
					size="icon"
					aria-label={t("theme.appearance")}
					title={t("theme.appearance")}
					className="hover:bg-sidebar-accent hover:text-sidebar-accent-foreground text-muted-foreground size-8 shrink-0 border-0 ring-offset-0 outline-none select-none focus-visible:ring-0"
					dataTestId="sidebar-theme"
				>
					<Sun className="size-4 scale-100 rotate-0 transition-all dark:scale-0 dark:-rotate-90" strokeWidth={2} />
					<Moon className="absolute size-4 scale-0 rotate-90 transition-all dark:scale-100 dark:rotate-0" strokeWidth={2} />
					<span className="sr-only">{t("theme.appearance")}</span>
				</Button>
			</DropdownMenuTrigger>
			<DropdownMenuContent align="end" className="w-40">
				<DropdownMenuLabel>{t("theme.appearance")}</DropdownMenuLabel>
				<DropdownMenuItem onClick={() => setTheme("light")}>{t("theme.light")}</DropdownMenuItem>
				<DropdownMenuItem onClick={() => setTheme("dark")}>{t("theme.dark")}</DropdownMenuItem>
				<DropdownMenuItem onClick={() => setTheme("system")}>{t("theme.system")}</DropdownMenuItem>

				<DropdownMenuSeparator />
				<DropdownMenuLabel>{t("language.label")}</DropdownMenuLabel>
				<DropdownMenuItem
					onClick={() => setLocale("zh-CN")}
					className="gap-2"
					data-testid="sidebar-language-zh"
				>
					<span>{t("language.zh")}</span>
					{locale === "zh-CN" && <Check className="text-muted-foreground ml-auto size-3.5" aria-hidden="true" />}
				</DropdownMenuItem>
				<DropdownMenuItem
					onClick={() => setLocale("en-US")}
					className="gap-2"
					data-testid="sidebar-language-en"
				>
					<span>{t("language.en")}</span>
					{locale === "en-US" && <Check className="text-muted-foreground ml-auto size-3.5" aria-hidden="true" />}
				</DropdownMenuItem>

				<DropdownMenuSeparator />
				<DropdownMenuLabel>{t("theme.color")}</DropdownMenuLabel>
				{PALETTES.map((p) => (
					<DropdownMenuItem key={p.key} onClick={() => setPalette(p.key)} className="gap-2">
						{/* 色点用 palette 自己的预览色，不受当前 --primary 影响 */}
						<span className="size-3.5 shrink-0 rounded-full" style={{ backgroundColor: p.preview }} aria-hidden="true" />
						<span>{p.label}</span>
						{palette === p.key && <Check className="text-muted-foreground ml-auto size-3.5" aria-hidden="true" />}
					</DropdownMenuItem>
				))}
			</DropdownMenuContent>
		</DropdownMenu>
	);
}
