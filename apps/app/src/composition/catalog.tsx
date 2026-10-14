import type { ReactNode } from 'react';
import type { NotificationNavigationTarget } from '@yuanpu-agent/protocol';
import { ScheduleManagement } from '../management.js';
import { createUiRegistry, type UiDestination } from '../ui-registry.js';
import { ChatPanel } from '../modules/chat.js';
import { SkillPage } from '../modules/skills.js';
import { KnowledgePage } from '../modules/knowledge.js';
import { ServerCenterPage } from '../modules/server-center.js';
import { SettingsPage } from '../modules/settings.js';
import { DesktopPage } from '../modules/desktop.js';

type AppView = UiDestination;

interface PageContext {
  active: boolean;
  configRoot: string;
  notificationTarget?: NotificationNavigationTarget;
  selectedScheduleId?: string;
  openScheduledRun: (runId: string, conversationId: string, scheduleId: string) => void;
  assistantWorkConversationId?: string;
  openAssistantWorkConversation: (conversationId: string) => void;
  clearAssistantWorkConversation: () => void;
  navigate: (view: AppView) => void;
}

export const uiRegistry = createUiRegistry<(context: PageContext) => ReactNode>();
uiRegistry.register({ id: 'work', label: '工作', order: 10, render: ({ active, notificationTarget, selectedScheduleId,
  assistantWorkConversationId, clearAssistantWorkConversation, navigate }) =>
  <ChatPanel active={active} surface="work" navigationTarget={notificationTarget}
    assistantWorkConversationId={assistantWorkConversationId}
    clearAssistantWorkConversation={clearAssistantWorkConversation}
    scheduleOrigin={Boolean(selectedScheduleId)} onReturnToSchedules={() => navigate('schedules')} /> });
uiRegistry.register({ id: 'desktop', label: '桌面', order: 15, render: ({ active, navigate }) =>
  <DesktopPage active={active} onOpen={navigate} /> });
uiRegistry.register({ id: 'assistant', label: '助理', order: 20, render: ({ active, openAssistantWorkConversation }) =>
  <ChatPanel active={active} surface="assistant" scheduleOrigin={false}
    onOpenWorkConversation={openAssistantWorkConversation} onReturnToSchedules={() => undefined} /> });
uiRegistry.register({ id: 'skills', label: '技能', order: 30, render: ({ active }) => <SkillPage active={active} /> });
uiRegistry.register({ id: 'schedules', label: '定时任务', order: 40, render: ({ active, selectedScheduleId, openScheduledRun }) =>
  <ScheduleManagement active={active} requestedScheduleId={selectedScheduleId} onOpenRun={openScheduledRun} /> });
uiRegistry.register({ id: 'knowledge', label: '知识库', order: 50, render: ({ active }) => <KnowledgePage active={active} /> });
uiRegistry.register({ id: 'server', label: '元朴中心', order: 55, render: ({ active }) => <ServerCenterPage active={active} /> });
uiRegistry.register({ id: 'settings', label: '设置', order: 60, render: ({ active, configRoot }) => <SettingsPage active={active} configRoot={configRoot} /> });
