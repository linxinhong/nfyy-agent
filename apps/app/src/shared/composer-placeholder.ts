import type { KeyboardEvent } from 'react';

export function acceptComposerPlaceholder(
  event: KeyboardEvent<HTMLTextAreaElement>,
  setText: (text: string) => void,
): boolean {
  const textarea = event.currentTarget;
  const placeholder = textarea.placeholder;
  if (event.key !== 'Tab' || event.shiftKey || event.altKey || event.ctrlKey || event.metaKey
    || event.nativeEvent.isComposing || event.keyCode === 229
    || textarea.disabled || textarea.value !== '' || !placeholder) return false;

  event.preventDefault();
  setText(placeholder);
  requestAnimationFrame(() => {
    if (textarea.isConnected && textarea.value === placeholder) {
      textarea.setSelectionRange(placeholder.length, placeholder.length);
    }
  });
  return true;
}
