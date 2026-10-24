import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { CommandDialog } from "./command";

describe("CommandDialog", () => {
	it("does not keep its accessible heading mounted while closed", () => {
		const html = renderToStaticMarkup(
			<CommandDialog open={false} title="命令栏" description="搜索页面">
				<div data-testid="command-content">content</div>
			</CommandDialog>,
		);

		expect(html).not.toContain("命令栏");
		expect(html).not.toContain("搜索页面");
		expect(html).not.toContain("command-content");
	});
});
