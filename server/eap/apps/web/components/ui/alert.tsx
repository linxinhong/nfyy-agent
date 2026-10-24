import { cva, type VariantProps } from "class-variance-authority";
import * as React from "react";

import { cn } from "@/lib/utils";

/**
 * Alert —— 四个语义轴走 token。
 *
 * bifrost 的版本每个 variant 都要写 6 处调色板值加暗色变体
 * （border-red-200/30 bg-red-50/30 text-red-900 dark:border-red-800/30 …），
 * 而且缺 success。改走 token 后每个 variant 缩成一行，且暗色自动跟随 ——
 * 因为 -muted / -foreground 这两个变量在 .dark 里已被重定义。
 */
const alertVariants = cva(
	"relative w-full rounded-sm border px-4 py-3 text-sm grid has-[>svg]:grid-cols-[calc(var(--spacing)*4)_1fr] grid-cols-[0_1fr] has-[>svg]:gap-x-3 gap-y-0.5 items-start [&>svg]:size-4 [&>svg]:translate-y-0.5 [&>svg]:text-current",
	{
		variants: {
			variant: {
				default: "bg-card text-card-foreground",
				success: "border-success/30 bg-success-muted text-success-foreground",
				warning: "border-warning/30 bg-warning-muted text-warning-foreground",
				destructive: "border-destructive/30 bg-destructive-muted text-destructive-foreground",
				info: "border-info/30 bg-info-muted text-info-foreground",
			},
		},
		defaultVariants: {
			variant: "default",
		},
	},
);

function Alert({ className, variant, ...props }: React.ComponentProps<"div"> & VariantProps<typeof alertVariants>) {
	return <div data-slot="alert" role="alert" className={cn(alertVariants({ variant }), className)} {...props} />;
}

function AlertTitle({ className, ...props }: React.ComponentProps<"div">) {
	return (
		<div data-slot="alert-title" className={cn("col-start-2 line-clamp-1 min-h-4 font-medium tracking-tight", className)} {...props} />
	);
}

function AlertDescription({ className, ...props }: React.ComponentProps<"div">) {
	return (
		<div
			data-slot="alert-description"
			// 继承父级的语义前景色（非 default variant 下），default 下退回 muted
			className={cn("col-start-2 grid justify-items-start gap-1 text-sm opacity-90 [&_p]:leading-relaxed", className)}
			{...props}
		/>
	);
}

export { Alert, AlertDescription, AlertTitle };
