export type UiDestination = 'desktop' | 'work' | 'assistant' | 'skills' | 'schedules' | 'knowledge' | 'server' | 'settings';

export interface UiContribution<T> {
  id: UiDestination;
  label: string;
  order: number;
  render: T;
}

/** A renderer-local lifecycle for built-in UI contributions. */
export function createUiRegistry<T>() {
  const entries = new Map<UiDestination, UiContribution<T>>();
  const listeners = new Set<() => void>();
  let snapshot: readonly UiContribution<T>[] = [];

  const publish = () => {
    snapshot = [...entries.values()].sort((a, b) => a.order - b.order);
    for (const listener of listeners) listener();
  };

  return {
    register(contribution: UiContribution<T>): () => void {
      if (entries.has(contribution.id)) throw new Error(`Duplicate UI contribution: ${contribution.id}`);
      entries.set(contribution.id, contribution);
      publish();
      return () => {
        if (entries.get(contribution.id) !== contribution) return;
        entries.delete(contribution.id);
        publish();
      };
    },
    getSnapshot: () => snapshot,
    subscribe(listener: () => void): () => void {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
  };
}
