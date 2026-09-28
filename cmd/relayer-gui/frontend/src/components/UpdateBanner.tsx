import type { UpdateInfo } from "../types/relayer";

interface UpdateBannerProps {
  update: UpdateInfo;
  onDownload(): void;
  onDismiss(): void;
}

export function UpdateBanner({ update, onDownload, onDismiss }: UpdateBannerProps) {
  return (
    <div className="update-banner" role="status">
      <span>
        <strong>Relayer {update.latest}</strong> is available. You are running {update.current}.
      </span>
      <div className="update-banner__actions">
        <button className="button button--primary button--small" type="button" onClick={onDownload}>
          Download
        </button>
        <button className="button button--ghost button--small" type="button" onClick={onDismiss}>
          Not now
        </button>
      </div>
    </div>
  );
}
