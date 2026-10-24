import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import * as React from "react";

import { cn } from "@/lib/utils";
import { Loader2 } from "lucide-react";

const buttonVariants = cva(
	"inline-flex items-center ring-none justify-center gap-2 whitespace-nowrap rounded-sm text-sm font-medium transition-all disabled:pointer-events-none disabled:opacity-50 [&_svg]:pointer-events-none [&_svg:not([class*='size-'])]:size-4 shrink-0 [&_svg]:shrink-0 outline-none focus-visible:ring-1 focus-visible:ring-offset-1 aria-invalid:border-destructive active:scale-[0.99] transition-transform duration-100",
	{
		variants: {
			variant: {
				default: "bg-primary text-primary-foreground hover:bg-primary/90 focus-visible:ring-primary/50",
				destructive:
					"bg-destructive text-white font-normal hover:bg-destructive/90 focus-visible:ring-destructive/50 dark:bg-destructive/60",
				outline:
					"border bg-background hover:bg-accent hover:text-accent-foreground focus-visible:ring-ring/50 dark:bg-input/30 dark:border-input dark:hover:bg-input/50",
				secondary: "bg-secondary text-secondary-foreground hover:bg-secondary/80 focus-visible:ring-secondary-foreground/30",
				ghost: "hover:bg-accent hover:text-accent-foreground focus-visible:ring-accent-foreground/30 dark:hover:bg-accent/50",
				link: "text-primary underline-offset-4 hover:underline focus-visible:ring-primary/50",
			},
			// 尺寸阶梯 xs 28 / sm 30 / default 32 / lg 40 —— 单调递增。
			//
			// bifrost 这里是反的（default h-7.5=30px 比 sm h-8=32px 更矮）。实测它
			// size="sm" 用了 238 次、显式 size="default" 只有 1 次、lg 是 0 次 ——
			// 真实主流高度其实是 32px，而 default 反而最少用。
			// 模板把 32px 定为 default，让「不传 size」直接得到主流值。
			size: {
				xs: "h-7 gap-1 rounded-sm px-2 text-xs has-[>svg]:px-1.5",
				sm: "h-7.5 gap-1.5 rounded-sm px-2.5 has-[>svg]:px-2",
				default: "h-8 px-3 py-1 has-[>svg]:px-2.5",
				lg: "h-10 rounded-sm px-6 has-[>svg]:px-4",
				icon: "size-8",
				"icon-sm": "size-7.5",
			},
		},
		defaultVariants: {
			variant: "default",
			size: "default",
		},
	},
);

function Button({
	className,
	variant,
	size,
	asChild = false,
	children,
	isLoading = false,
	dataTestId,
	...props
}: React.ComponentProps<"button"> &
	VariantProps<typeof buttonVariants> & {
		asChild?: boolean;
		isLoading?: boolean;
		dataTestId?: string;
	}) {
	return (
		<BaseButton className={className} variant={variant} size={size} asChild={asChild} dataTestId={dataTestId} {...props}>
			{isLoading ? <Loader2 className="size-4 animate-spin" /> : children}
		</BaseButton>
	);
}

function BaseButton({
	className,
	variant,
	size,
	asChild = false,
	dataTestId,
	...props
}: React.ComponentProps<"button"> &
	VariantProps<typeof buttonVariants> & {
		asChild?: boolean;
		dataTestId?: string;
	}) {
	const Comp = asChild ? Slot : "button";

	return (
		<Comp
			data-slot="button"
			data-testid={dataTestId}
			className={cn(buttonVariants({ variant, size, className }), "cursor-pointer")}
			{...props}
		/>
	);
}

export { Button, buttonVariants };
