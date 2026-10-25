const SCROLL_IDLE_MS = 500;

export function installAutoFadingScrollbars() {
  const idleTimers = new Map<HTMLElement, number>();

  const onScroll = (event: Event) => {
    const element = event.target;
    if (!(element instanceof HTMLElement) || !element.closest('.app-shell')) return;
    if (getComputedStyle(element).scrollbarWidth === 'none') return;

    element.dataset.ypScrollbar = 'active';
    const previousTimer = idleTimers.get(element);
    if (previousTimer !== undefined) window.clearTimeout(previousTimer);
    idleTimers.set(element, window.setTimeout(() => {
      element.dataset.ypScrollbar = 'idle';
      idleTimers.delete(element);
    }, SCROLL_IDLE_MS));
  };

  // Scroll does not bubble, so capture scroll events from nested panes as well.
  document.addEventListener('scroll', onScroll, true);
  return () => {
    document.removeEventListener('scroll', onScroll, true);
    for (const timer of idleTimers.values()) window.clearTimeout(timer);
    idleTimers.clear();
  };
}
