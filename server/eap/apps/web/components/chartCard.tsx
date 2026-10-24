import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import type { ReactNode } from "react";

/**
 * ChartCard —— 「loading 有形状」的载体。
 *
 * 三个不变量，缺一个这个模式就失效：
 *   1. 卡片高度固定（h-[260px] sm:h-[330px]）—— 数据到达时零布局跳动
 *   2. Header 在 loading 与就绪两个分支**完全一致** —— 标题、控件、总数不闪现
 *   3. 只有数据区换成骨架
 *
 * 常见的错误做法是 `loading ? <Spinner/> : <Chart/>`：整块被替换，
 * 于是标题消失、高度塌陷、数据到达时页面跳一下。那不是 loading 状态，
 * 是"页面还不存在"状态。
 *
 * data-testid 派生约定：壳自动生成 `${testId}-total`、`-chart-skeleton`、`-actions`。
 * 这些是 E2E 的契约 —— 改名前必须 grep 测试目录。
 */
interface ChartCardProps {
	title: string;
	children: ReactNode;
	/** 右上角的筛选/切换控件 */
	controls?: ReactNode;
	/** 标题下方的图例行 */
	legend?: ReactNode;
	loading?: boolean;
	testId?: string;
	className?: string;
	/** 左上角的汇总数字 */
	total?: ReactNode;
	totalLabel?: string;
	totalTooltip?: ReactNode;
}

// 数据区用 `min-h-0 grow`：min-h-0 不可省 —— flex 子项默认 min-height:auto，
// 缺了它 ResponsiveContainer 量到的是内容高度而非剩余空间，recharts 报 width(-1) height(-1)。
const CARD_CLASS = "min-w-0 rounded-sm p-2 shadow-none h-[260px] sm:h-[330px]";

function TotalChip({
	total,
	totalLabel,
	totalTooltip,
	testId,
}: {
	total: ReactNode;
	totalLabel?: string;
	totalTooltip?: ReactNode;
	testId?: string;
}) {
	const chip = (
		<span
			className="text-muted-foreground flex shrink-0 items-baseline gap-1 pl-2 text-xs"
			data-testid={testId ? `${testId}-total` : undefined}
		>
			{totalLabel && <span>{totalLabel}</span>}
			<span className="text-primary text-sm font-semibold tabular-nums">{total}</span>
		</span>
	);

	if (totalTooltip === undefined || totalTooltip === null) return chip;

	return (
		<Tooltip>
			<TooltipTrigger asChild>
				{/* tabIndex 让 tooltip 可键盘触发 —— 纯 hover 触发对键盘用户不可达 */}
				<span tabIndex={0}>{chip}</span>
			</TooltipTrigger>
			<TooltipContent>{totalTooltip}</TooltipContent>
		</Tooltip>
	);
}

function Header({
	title,
	controls,
	legend,
	total,
	totalLabel,
	totalTooltip,
	testId,
}: Omit<ChartCardProps, "children" | "loading" | "className">) {
	const hasTotal = total !== undefined && total !== null;
	const hasActionRow = hasTotal || controls;

	return (
		<div className="shrink-0 space-y-2">
			<div className="pr-1 pl-2">
				<span className="text-primary text-sm font-medium">{title}</span>
			</div>
			{hasActionRow && (
				<div
					className="flex min-h-7 w-full min-w-0 flex-wrap items-center justify-between gap-2 sm:gap-3"
					data-testid={testId ? `${testId}-actions` : undefined}
				>
					{hasTotal ? (
						<TotalChip total={total} totalLabel={totalLabel} totalTooltip={totalTooltip} testId={testId} />
					) : (
						/* 占位保持 justify-between 的两端对齐，否则 controls 会跳到左侧 */
						<span className="shrink-0" />
					)}
					{controls && <div className="flex shrink-0 items-center gap-2">{controls}</div>}
				</div>
			)}
			{legend && <div className="w-full min-w-0">{legend}</div>}
		</div>
	);
}

export function ChartCard({
	title,
	children,
	controls,
	legend,
	loading,
	testId,
	className,
	total,
	totalLabel,
	totalTooltip,
}: ChartCardProps) {
	const header = (
		<Header
			title={title}
			controls={controls}
			legend={legend}
			total={total}
			totalLabel={totalLabel}
			totalTooltip={totalTooltip}
			testId={testId}
		/>
	);

	return (
		<Card className={cn(CARD_CLASS, className)} data-testid={testId}>
			{header}
			{loading ? (
				<div className="min-h-0 grow" data-testid={testId ? `${testId}-chart-skeleton` : undefined}>
					<Skeleton className="h-full w-full" />
				</div>
			) : (
				<div className="min-h-0 grow">{children}</div>
			)}
		</Card>
	);
}
