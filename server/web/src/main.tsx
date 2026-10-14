import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { CenterApp } from './center-app.js';
import './style.css';
import './portal.css';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <CenterApp />
  </StrictMode>,
);
