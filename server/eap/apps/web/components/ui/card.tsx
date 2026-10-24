import * as React from "react";

import { cn } from "@/lib/utils";

/**
 * Card —— 密度对齐整体基调（12px 内边距），且**无阴影**。
 *
 * 两处对 shadcn 默认值的修正（bifrost 原封不动照抄了默认值）：
 *
 *   1. py-6/px-6/gap-6（24px）→ py-3/px-3/gap-3（12px）。24px 与整体控件密度
 *      （28/30/32px 五档）不符，结果 bifrost 业务里几乎每处都要覆盖成 p-2。
 *      默认值就该是最常用的值。
 *   2. 去掉 shadow-sm。层次靠 border + 间距 + 底色差，全程零阴影 —— 这是
 *      三层层次成立的前提，加了阴影会让内嵌卡片"浮起来"破坏平整感。
 */
function Card({ className, ...props }: React.ComponentProps<"div">) {
	return (
		<div
			data-slot="card"
			className={cn("bg-card text-card-foreground flex flex-col gap-3 rounded-sm border py-3 shadow-none", className)}
			{...props}
		/>
	);
}

function CardHeader({ className, ...props }: React.ComponentProps<"div">) {
	return (
		<div
			data-slot="card-header"
			className={cn(
				"@container/card-header grid auto-rows-min grid-rows-[auto_auto] items-start gap-1.5 px-3 has-data-[slot=card-action]:grid-cols-[1fr_auto] [.border-b]:pb-3",
				className,
			)}
			{...props}
		/>
	);
}

function CardTitle({ className, ...props }: React.ComponentProps<"div">) {
	return <div data-slot="card-title" className={cn("text-sm leading-none font-semibold", className)} {...props} />;
}

function CardDescription({ className, ...props }: React.ComponentProps<"div">) {
	return <div data-slot="card-description" className={cn("text-muted-foreground text-xs", className)} {...props} />;
}

function CardAction({ className, ...props }: React.ComponentProps<"div">) {
	return (
		<div data-slot="card-action" className={cn("col-start-2 row-span-2 row-start-1 self-start justify-self-end", className)} {...props} />
	);
}

function CardContent({ className, ...props }: React.ComponentProps<"div">) {
	return <div data-slot="card-content" className={cn("px-3", className)} {...props} />;
}

function CardFooter({ className, ...props }: React.ComponentProps<"div">) {
	return <div data-slot="card-footer" className={cn("flex items-center px-3 [.border-t]:pt-3", className)} {...props} />;
}

export { Card, CardAction, CardContent, CardDescription, CardFooter, CardHeader, CardTitle };
