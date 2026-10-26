import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Upload, CheckCircle2, RefreshCw, Github, Cloud, HardDriveUpload } from 'lucide-react';
import { warehouseApi } from '@/api/client';
import { ApiError } from '@/lib/api';
import { Button } from '@/components/ui/button';
import { Card } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { showToast } from '@/components/ui/ToastProvider';
import { cn } from '@/lib/utils';

type ImportSource = 'github' | 'cnb' | 'upload';

export function ImportTab({ onDone, onCancel }: { onDone: () => void; onCancel: () => void }) {
  const { t } = useTranslation('warehouse');
  const navigate = useNavigate();
  const [source, setSource] = useState<ImportSource>('github');
  const [repo, setRepo] = useState('TencentCloud/CubeSandbox');
  const [tag, setTag] = useState('');
  const [arch, setArch] = useState<string[]>(['amd64']);
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState(false);

  const toggleArch = (a: string) => {
    setArch((cur) => (cur.includes(a) ? cur.filter((x) => x !== a) : [...cur, a]));
  };

  const submit = async () => {
    setBusy(true);
    try {
      let uploadId: string | undefined;
      if (source === 'upload') {
        if (!file) throw new Error(t('uploadFile'));
        showToast(t('uploading'));
        const up = await warehouseApi.upload(file);
        uploadId = up.uploadId;
      }
      await warehouseApi.createImport({
        source,
        repo: source === 'upload' ? undefined : repo,
        tag: source === 'upload' ? undefined : tag,
        uploadId,
        arch,
      });
      showToast(t('importSubmitted'), 'success');
      onDone();
      navigate('/warehouse/jobs?tab=import');
    } catch (err) {
      const disabled =
        err instanceof ApiError &&
        (err.status === 501 ||
          (typeof err.body === 'object' &&
            err.body !== null &&
            'code' in err.body &&
            (err.body as { code?: string }).code === 'warehouse_disabled'));
      showToast(
        disabled ? t('disabled') : err instanceof ApiError ? err.message : String(err),
        'warn',
      );
    } finally {
      setBusy(false);
    }
  };

  const sources: { id: ImportSource; icon: React.ElementType; label: string; desc: string }[] = [
    { id: 'github', icon: Github, label: 'GitHub Release', desc: 'Pull from GitHub' },
    { id: 'cnb', icon: Cloud, label: 'CNB Release', desc: 'Pull from CNB' },
    { id: 'upload', icon: HardDriveUpload, label: 'Local File', desc: 'Upload .tar.gz' },
  ];

  return (
    <Card className="flex flex-col overflow-hidden border-0 shadow-2xl sm:rounded-xl">
      <div className="bg-muted/30 px-6 py-4 border-b border-border/50">
        <h2 className="text-lg font-semibold">{t('import')}</h2>
        <p className="text-xs text-muted-foreground mt-1">
          Import a one-click component package into the warehouse
        </p>
      </div>

      <div className="p-6 space-y-6">
        <div>
          <label className="text-xs font-semibold uppercase tracking-wider text-muted-foreground mb-3 block">
            Source
          </label>
          <div className="grid grid-cols-3 gap-3">
            {sources.map((s) => {
              const Icon = s.icon;
              const isActive = source === s.id;
              return (
                <button
                  key={s.id}
                  onClick={() => {
                    if (s.id === 'github' && repo === 'CubeSandbox/CubeSandbox') {
                      setRepo('TencentCloud/CubeSandbox');
                    } else if (s.id === 'cnb' && repo === 'TencentCloud/CubeSandbox') {
                      setRepo('CubeSandbox/CubeSandbox');
                    }
                    setSource(s.id);
                  }}
                  className={cn(
                    'flex flex-col items-center justify-center p-3 rounded-lg border text-center transition-all',
                    isActive
                      ? 'border-primary bg-primary/5 text-primary ring-1 ring-primary/20'
                      : 'border-border/60 hover:bg-muted/50 hover:border-border text-muted-foreground',
                  )}
                >
                  <Icon size={20} className={cn('mb-2', isActive ? 'text-primary' : '')} />
                  <span
                    className={cn('text-[11px] font-medium', isActive ? 'text-foreground' : '')}
                  >
                    {t(`source.${s.id}`)}
                  </span>
                </button>
              );
            })}
          </div>
        </div>

        <div className="space-y-4">
          {source !== 'upload' && (
            <div className="grid grid-cols-2 gap-4">
              <label className="block space-y-1.5">
                <span className="text-xs font-medium text-foreground">{t('repo')}</span>
                <Input
                  value={repo}
                  onChange={(e) => setRepo(e.target.value)}
                  className="h-9 text-sm"
                />
              </label>
              <label className="block space-y-1.5">
                <span className="text-xs font-medium text-foreground">{t('tag')}</span>
                <Input
                  value={tag}
                  onChange={(e) => setTag(e.target.value)}
                  placeholder="v0.6.0"
                  className="h-9 text-sm"
                />
              </label>
            </div>
          )}

          {source === 'upload' && (
            <label className="block space-y-1.5">
              <span className="text-xs font-medium text-foreground">{t('uploadFile')}</span>
              <div className="flex items-center justify-center w-full">
                <label className="flex flex-col items-center justify-center w-full h-32 border-2 border-dashed border-border/60 rounded-lg cursor-pointer bg-muted/20 hover:bg-muted/40 transition-colors">
                  <div className="flex flex-col items-center justify-center pt-5 pb-6">
                    <Upload className="w-8 h-8 mb-3 text-muted-foreground" />
                    <p className="mb-1 text-sm text-muted-foreground">
                      <span className="font-semibold text-foreground">Click to upload</span> or drag
                      and drop
                    </p>
                    <p className="text-xs text-muted-foreground">.tar.gz or .tgz files only</p>
                    {file && <p className="mt-2 text-sm font-medium text-primary">{file.name}</p>}
                  </div>
                  <input
                    type="file"
                    className="hidden"
                    accept=".tar.gz,.tgz"
                    onChange={(e) => setFile(e.target.files?.[0] ?? null)}
                  />
                </label>
              </div>
            </label>
          )}

          <div className="pt-2">
            <label className="text-xs font-semibold uppercase tracking-wider text-muted-foreground mb-3 block">
              Architectures
            </label>
            <div className="flex gap-4">
              {['amd64', 'arm64'].map((a) => (
                <label key={a} className="flex items-center gap-2.5 cursor-pointer">
                  <div
                    className={cn(
                      'flex h-4 w-4 items-center justify-center rounded border transition-colors',
                      arch.includes(a)
                        ? 'bg-primary border-primary text-primary-foreground'
                        : 'border-input',
                    )}
                  >
                    {arch.includes(a) && <CheckCircle2 size={12} strokeWidth={3} />}
                  </div>
                  <input
                    type="checkbox"
                    className="hidden"
                    checked={arch.includes(a)}
                    onChange={() => toggleArch(a)}
                  />
                  <span className="text-sm font-medium">{a}</span>
                </label>
              ))}
            </div>
            {arch.length === 0 && (
              <p className="text-[11px] text-destructive mt-1.5">
                Please select at least one architecture.
              </p>
            )}
          </div>
        </div>
      </div>

      <div className="bg-muted/30 px-6 py-4 border-t border-border/50 flex justify-end gap-3">
        <Button variant="outline" onClick={onCancel} disabled={busy} className="h-9">
          {t('cancel')}
        </Button>
        <Button
          disabled={busy || arch.length === 0 || (source === 'upload' && !file)}
          onClick={() => void submit()}
          className="h-9"
        >
          {busy ? (
            <>
              <RefreshCw size={14} className="mr-2 animate-spin" />{' '}
              {source === 'upload' ? t('uploading') : t('submitting')}
            </>
          ) : (
            <>
              <Upload size={14} className="mr-2" /> {t('submit')}
            </>
          )}
        </Button>
      </div>
    </Card>
  );
}
