CREATE TABLE yuanpu_role_permissions (
    role text NOT NULL CHECK (role IN ('admin', 'reviewer', 'team_lead', 'member')),
    action text NOT NULL CHECK (action IN ('create', 'read', 'edit', 'submit', 'review', 'withdraw')),
    scope text NOT NULL CHECK (scope IN ('none', 'own', 'team', 'all')),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (role, action)
);

INSERT INTO yuanpu_role_permissions (role,action,scope) VALUES
('admin','create','all'),('admin','read','all'),('admin','edit','own'),('admin','submit','own'),('admin','review','all'),('admin','withdraw','all'),
('reviewer','create','all'),('reviewer','read','all'),('reviewer','edit','own'),('reviewer','submit','own'),('reviewer','review','all'),('reviewer','withdraw','none'),
('team_lead','create','all'),('team_lead','read','team'),('team_lead','edit','own'),('team_lead','submit','own'),('team_lead','review','team'),('team_lead','withdraw','none'),
('member','create','all'),('member','read','team'),('member','edit','own'),('member','submit','own'),('member','review','none'),('member','withdraw','none');

ALTER TABLE yuanpu_resource_revisions ADD COLUMN author_confirmed_at timestamptz;
