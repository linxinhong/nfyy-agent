export type WordEditorMode = 'edit' | 'readonly';

/** Serializable descriptor; filesystem and process operations stay in the host. */
export interface WordEditorDocument {
  id: string;
  path: string;
  title: string;
  mode: WordEditorMode;
  /** frame lets the UI import the document; model prepares it for headless tools. */
  load: 'frame' | 'model';
  url: string;
  origin: string;
}

export interface WordEditorStatus {
  dirty: boolean;
  lastSavedAt?: number;
}

export interface WordTextLocation {
  begin: number;
  end: number;
}

export class WordEditorError extends Error {
  constructor(public readonly code: string, message: string) {
    super(message);
    this.name = 'WordEditorError';
  }
}
