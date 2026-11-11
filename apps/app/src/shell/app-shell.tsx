import { useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from 'react';
import { useLocation, useNavigate } from 'react-router';
import type { NotificationNavigationTarget, RuntimeRecoveryNotice } from '@yuanpu-agent/protocol';
import type { UiDestination } from '../ui-registry.js';
import { AppIcon } from '../shared/app-icon.js';
import { AvatarMark } from '../shared/avatar-mark.js';
import { BrandShowcase } from '../shared/brand-showcase.js';
import { emo, showNavigation } from '../emo.js';

import { PageToolbarHost } from '../shared/page-toolbar.js';

import { uiRegistry } from '../composition/catalog.js';

type AppView = UiDestination;

export function App() {
  const [toolbarHost, setToolbarHost] = useState<HTMLDivElement | null>(null);
  const location = useLocation();
  const routerNavigate = useNavigate();
  const destinations = useSyncExternalStore(uiRegistry.subscribe, uiRegistry.getSnapshot);
  const enabledDestinations = destinations.filter((entry) => showNavigation(entry.id));
  const routeId = location.pathname.slice(1).split('/')[0];
  const fallbackView = enabledDestinations.find((entry) => entry.id !== 'settings')?.id ?? 'settings';
  const view: AppView = enabledDestinations.some((entry) => entry.id === routeId) ? routeId as AppView : fallbackView;
  const setView = (destination: AppView) => routerNavigate(`/${showNavigation(destination) ? destination : fallbackView}`);
  // The desktop canvas is a test entry of the default light/dark navigation only, not the Yuanpu rail.
  const moreDestinations = enabledDestinations.filter((entry) => entry.id !== 'work' && entry.id !== 'desktop' && entry.id !== 'settings');
  const [openMenu, setOpenMenu] = useState<'more' | 'personal' | null>(null);
  const [menuPosition, setMenuPosition] = useState<{ left: number; top: number } | null>(null);
  const moreTrigger = useRef<HTMLButtonElement>(null);
  const personalTrigger = useRef<HTMLButtonElement>(null);
  const menuElement = useRef<HTMLDivElement>(null);
  const [configRoot, setConfigRoot] = useState('~/.yuanpu');
  const [notificationTarget, setNotificationTarget] = useState<NotificationNavigationTarget>();
  const [selectedScheduleId, setSelectedScheduleId] = useState<string>();
  const [assistantWorkConversationId, setAssistantWorkConversationId] = useState<string>();
  const [runtimeRecoveryNotice, setRuntimeRecoveryNotice] = useState<RuntimeRecoveryNotice>();

  useLayoutEffect(() => {
    setOpenMenu(null);
  }, [location.pathname]);

  useLayoutEffect(() => {
    if (!openMenu) {
      setMenuPosition(null);
      return;
    }
    const trigger = openMenu === 'more' ? moreTrigger.current : personalTrigger.current;
    const menu = menuElement.current;
    if (!trigger || !menu) return;
    const positionMenu = () => {
      const triggerRect = trigger.getBoundingClientRect();
      const menuRect = menu.getBoundingClientRect();
      const narrow = window.matchMedia('(max-width: 780px)').matches;
      const desiredLeft = narrow ? triggerRect.left : triggerRect.right + 12;
      const desiredTop = narrow
        ? triggerRect.top - menuRect.height - 10
        : openMenu === 'personal' ? triggerRect.bottom - menuRect.height : triggerRect.top - 4;
      setMenuPosition({
        left: Math.max(8, Math.min(desiredLeft, window.innerWidth - menuRect.width - 8)),
        top: Math.max(8, Math.min(desiredTop, window.innerHeight - menuRect.height - 8)),
      });
    };
    positionMenu();
    window.addEventListener('resize', positionMenu);
    return () => window.removeEventListener('resize', positionMenu);
  }, [openMenu, moreDestinations.length]);

  useLayoutEffect(() => {
    if (openMenu && menuPosition) menuElement.current?.querySelector<HTMLButtonElement>('[role="menuitem"]')?.focus();
  }, [openMenu, menuPosition]);

  useEffect(() => {
    if (!openMenu) return;
    const dismissOutside = (event: PointerEvent) => {
      const target = event.target as Node;
      if (!menuElement.current?.contains(target) && !moreTrigger.current?.contains(target) && !personalTrigger.current?.contains(target)) {
        setOpenMenu(null);
      }
    };
    const dismissEscape = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return;
      event.preventDefault();
      (openMenu === 'more' ? moreTrigger : personalTrigger).current?.focus();
      setOpenMenu(null);
    };
    document.addEventListener('pointerdown', dismissOutside);
    document.addEventListener('keydown', dismissEscape);
    return () => {
      document.removeEventListener('pointerdown', dismissOutside);
      document.removeEventListener('keydown', dismissEscape);
    };
  }, [openMenu]);

  const openDestination = (destination: AppView) => {
    setOpenMenu(null);
    setView(destination);
  };

  const handleMenuKeyDown = (event: React.KeyboardEvent<HTMLDivElement>) => {
    if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
    const items = Array.from(menuElement.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]') ?? []);
    if (!items.length) return;
    event.preventDefault();
    const index = items.indexOf(document.activeElement as HTMLButtonElement);
    const next = event.key === 'Home' ? 0 : event.key === 'End' ? items.length - 1
      : event.key === 'ArrowDown' ? (index + 1) % items.length : (index - 1 + items.length) % items.length;
    items[next]?.focus();
  };

  useEffect(() => {
    void window.yuanpu?.runtimeInfo()
      .then((info) => setConfigRoot(info.configRoot))
      .catch(() => undefined);
  }, []);

  useEffect(() => {
    void window.yuanpu?.runtimeRecoveryNotice()
      .then(setRuntimeRecoveryNotice)
      .catch(() => undefined);
  }, []);

  useEffect(() => window.yuanpu?.onNotificationNavigation((target) => {
    setNotificationTarget(target);
    setSelectedScheduleId(undefined);
    routerNavigate('/work');
  }), [routerNavigate]);

  return (
    <PageToolbarHost.Provider value={toolbarHost}><main className="app-shell">
      <div className="shell-topbar">
        <div className="window-controls-space" aria-hidden="true">
          {!window.yuanpu && <div className="window-controls-preview"><span /><span /><span /></div>}
        </div>
        <div className="shell-toolbar-host" ref={setToolbarHost} />
      </div>
      <aside className="sidebar">
        <nav className="sidebar-nav compact-sidebar-nav" aria-label="主导航">
          <button className={`nav-item ${view === 'work' ? 'active' : ''}`} type="button"
            title="对话" aria-label="对话" aria-current={view === 'work' ? 'page' : undefined}
            onClick={() => openDestination('work')}>
            <AppIcon name="work" /><span className="nav-label">对话</span>
          </button>
          <button ref={moreTrigger} className={`nav-item ${openMenu === 'more' || moreDestinations.some((entry) => entry.id === view) ? 'active' : ''}`} type="button"
            title="更多" aria-label="更多" aria-haspopup="menu" aria-controls="sidebar-menu" aria-expanded={openMenu === 'more'}
            onClick={() => setOpenMenu((current) => current === 'more' ? null : 'more')}>
            <AppIcon name="more" /><span className="nav-label">更多</span>
          </button>
        </nav>
        <div className="compact-sidebar-bottom">
          <button ref={personalTrigger} className={`nav-item ${openMenu === 'personal' || view === 'settings' ? 'active' : ''}`} type="button"
            title="个人" aria-label="个人" aria-haspopup="menu" aria-controls="sidebar-menu" aria-expanded={openMenu === 'personal'}
            onClick={() => setOpenMenu((current) => current === 'personal' ? null : 'personal')}>
            <AppIcon name="person" /><span className="nav-label">个人</span>
          </button>
        </div>

        <nav className="sidebar-nav full-sidebar-nav" aria-label="主导航">{enabledDestinations.filter((entry) => entry.id !== 'settings').map((entry) =>
          <button key={entry.id} className={`nav-item ${view === entry.id ? 'active' : ''}`} type="button"
            title={entry.label} aria-label={entry.label} aria-current={view === entry.id ? 'page' : undefined} onClick={() => setView(entry.id)}>
            <AppIcon name={entry.id} /><span className="nav-label">{entry.label}</span>
          </button>)}</nav>

        <div className="sidebar-bottom full-sidebar-bottom">
          <BrandShowcase name={emo.brand.name} />
          <button className={`sidebar-footer settings-trigger ${view === 'settings' ? 'active' : ''}`} type="button"
            title="设置" aria-label="设置" onClick={() => setView('settings')}><AppIcon name="settings" /></button>
          <div className="profile-avatar" role="img" aria-label={`${emo.brand.name} 头像`}><AvatarMark /></div>
        </div>
      </aside>

      {openMenu && <div ref={menuElement} id="sidebar-menu" className="sidebar-menu" role="menu"
        aria-label={openMenu === 'more' ? '更多页面' : '个人菜单'} onKeyDown={handleMenuKeyDown}
        style={menuPosition ? { left: menuPosition.left, top: menuPosition.top } : { visibility: 'hidden' }}>
        {openMenu === 'more' ? <>
          <div className="sidebar-menu-title">更多</div>
          {moreDestinations.map((entry) => <button key={entry.id} className={`sidebar-menu-item ${view === entry.id ? 'active' : ''}`}
            role="menuitem" type="button" onClick={() => openDestination(entry.id)}>
            <AppIcon name={entry.id} /><span>{entry.label}</span>
          </button>)}
        </> : <>
          <div className="sidebar-menu-title">个人</div>
          <button className={`sidebar-menu-item ${view === 'settings' ? 'active' : ''}`} role="menuitem" type="button"
            onClick={() => openDestination('settings')}><AppIcon name="settings" /><span>设置</span></button>
        </>}
      </div>}

      {runtimeRecoveryNotice && (
        <div className="runtime-recovery-notice" role="alert">
          <div>
            <strong>{runtimeRecoveryNotice.kind === 'incompatible_protocol' ? 'Runtime 协议不兼容' : 'Runtime 更新失败'}</strong>
            <span>已恢复上一版本，当前 App 可继续使用。请检查更新后再重试。</span>
          </div>
          <button type="button" aria-label="关闭 Runtime 更新提示" onClick={() => setRuntimeRecoveryNotice(undefined)}>关闭</button>
        </div>
      )}

      {enabledDestinations.map((entry) => <div className={`page-host ${view === entry.id ? '' : 'view-hidden'}`} key={entry.id}>
        {entry.render({
          active: view === entry.id,
          configRoot,
          notificationTarget,
          selectedScheduleId,
          assistantWorkConversationId,
          openAssistantWorkConversation: (conversationId) => {
            setAssistantWorkConversationId(conversationId);
            setNotificationTarget(undefined);
            setSelectedScheduleId(undefined);
            setView('work');
          },
          clearAssistantWorkConversation: () => setAssistantWorkConversationId(undefined),
          navigate: setView,
          openScheduledRun: (runId, conversationId, scheduleId) => {
            setSelectedScheduleId(scheduleId);
            setNotificationTarget({ runId, conversationId });
            setView('work');
          },
        })}
      </div>)}
    </main></PageToolbarHost.Provider>
  );
}
