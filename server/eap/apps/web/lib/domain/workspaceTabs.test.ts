import { describe, expect, it } from "vitest";
import { closeDetailTab, LIST_TAB_ID, openDetailTab, type DetailTab } from "./workspaceTabs";

const a: DetailTab = { id: "call_1", label: "来电 A" };
const b: DetailTab = { id: "call_2", label: "外呼 B" };

describe("页内详情标签", () => {
	it("打开新标签追加并激活；重复打开只激活不新增", () => {
		const first = openDetailTab([], a);
		expect(first).toEqual({ tabs: [a], activeId: "call_1" });
		const second = openDetailTab(first.tabs, b);
		expect(second.tabs).toEqual([a, b]);
		expect(second.activeId).toBe("call_2");
		const again = openDetailTab(second.tabs, { ...a, label: "来电 A（重复）" });
		expect(again.tabs).toEqual([a, b]);
		expect(again.activeId).toBe("call_1");
	});

	it("关闭非激活标签不影响激活态", () => {
		const { close } = { close: closeDetailTab([a, b], "call_1", "call_2") };
		expect(close).toEqual({ tabs: [b], activeId: "call_2" });
	});

	it("关闭激活标签优先激活右邻，无右邻则左邻", () => {
		expect(closeDetailTab([a, b], "call_1", "call_1")).toEqual({ tabs: [b], activeId: "call_2" });
		expect(closeDetailTab([a, b], "call_2", "call_2")).toEqual({ tabs: [a], activeId: "call_1" });
	});

	it("关闭最后一个标签回到列表视图", () => {
		expect(closeDetailTab([a], "call_1", "call_1")).toEqual({ tabs: [], activeId: null });
	});

	it("关闭不存在的标签是幂等空操作", () => {
		expect(closeDetailTab([a], "call_x", "call_1")).toEqual({ tabs: [a], activeId: "call_1" });
	});

	it("列表视图固定标签 id 不与业务标签混淆", () => {
		expect(LIST_TAB_ID).toBe(":list");
	});
});
