import { useState } from 'react';
import { brandIconUrl } from '../emo.js';

export function BrandShowcase({ name = '南医小助' }: { name?: string }) {
  const [iconFailed, setIconFailed] = useState(false);
  const iconUrl = brandIconUrl();
  return <div className="brand-showcase" aria-label={name}>
    <span className="brand-showcase-mark" aria-hidden="true">
      {iconUrl && !iconFailed ? <img src={iconUrl} alt="" onError={() => setIconFailed(true)} />
        : <svg viewBox="0 0 28 28" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
          <circle cx="8" cy="8" r="2.5" /><circle cx="20" cy="8" r="2.5" /><circle cx="14" cy="20" r="2.5" />
          <path d="m10.2 9.5 2.5 7.7m5.1-7.7-2.5 7.7M10.5 8h7" />
        </svg>}
    </span>
  </div>;
}
