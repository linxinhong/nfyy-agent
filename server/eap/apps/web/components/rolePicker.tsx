import { ChevronDown } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import type { Role } from "@/lib/api/mindlink";

/**
 * 多选角色：按钮显示已选角色名，弹层里勾选。只列出给定范围（全所或团队内）
 * 的角色；角色由管理员自定义，所以显示的是目录里的名称，不翻译角色键。
 */
export function RolePicker({
	roles,
	value,
	onChange,
	placeholder,
	disabled,
	testId,
}: {
	roles: Role[];
	value: string[];
	onChange: (next: string[]) => void;
	placeholder: string;
	disabled?: boolean;
	testId?: string;
}) {
	const names = roles.filter((role) => value.includes(role.key)).map((role) => role.name);
	const toggle = (key: string, on: boolean) => onChange(on ? [...value, key] : value.filter((k) => k !== key));
	return (
		<Popover>
			<PopoverTrigger asChild>
				<Button variant="outline" size="sm" className="h-8 max-w-48 justify-between font-normal" disabled={disabled} data-testid={testId}>
					<span className="truncate">{names.length > 0 ? names.join("、") : placeholder}</span>
					<ChevronDown className="opacity-50" />
				</Button>
			</PopoverTrigger>
			<PopoverContent className="flex w-56 flex-col gap-2 p-3" align="start">
				{roles.map((role) => (
					<Label key={role.key} className="flex items-center gap-2 font-normal">
						<Checkbox checked={value.includes(role.key)} onCheckedChange={(checked) => toggle(role.key, checked === true)} />
						{role.name}
					</Label>
				))}
			</PopoverContent>
		</Popover>
	);
}
