import { Component, DestroyRef, OnInit, computed, inject, signal } from '@angular/core';
import { DatePipe } from '@angular/common';
import { ApiService } from './core/api.service';
import { AuthService } from './core/auth.service';
import { Enrollment, EnrollmentStatus, LogEntry, Cohort } from './core/models';

const BADGES: Record<EnrollmentStatus, string> = {
  Requested: 'bg-slate-100 text-slate-700 ring-slate-300',
  Held: 'bg-amber-50 text-amber-800 ring-amber-300',
  Enrolled: 'bg-emerald-50 text-emerald-800 ring-emerald-300',
  Withdrawn: 'bg-sky-50 text-sky-800 ring-sky-300',
  Rejected: 'bg-rose-50 text-rose-800 ring-rose-300',
};

@Component({
  selector: 'app-workspace',
  imports: [DatePipe],
  templateUrl: './workspace.component.html',
})
export class WorkspaceComponent implements OnInit {
  private readonly api = inject(ApiService);
  readonly auth = inject(AuthService);
  private readonly destroyRef = inject(DestroyRef);

  readonly cohorts = signal<Cohort[]>([]);
  readonly selectedCode = signal('');
  readonly enrollments = signal<Enrollment[]>([]);
  readonly log = signal<LogEntry[]>([]);
  readonly logFor = signal<string | null>(null);
  readonly error = signal('');

  readonly code = signal('');
  readonly title = signal('Intro to Distributed Systems');
  readonly track = signal('Engineering');
  readonly instructor = signal('Ada Lovelace');
  readonly capacity = signal('12');

  readonly reference = signal('');
  readonly seats = signal('1');

  readonly selected = computed(() => this.cohorts().find((s) => s.code === this.selectedCode()));

  ngOnInit(): void {
    void this.refresh();
    const timer = setInterval(() => void this.refresh(), 1000);
    this.destroyRef.onDestroy(() => clearInterval(timer));
  }

  badge(status: EnrollmentStatus): string {
    return BADGES[status];
  }

  money(cents: number): string {
    return (cents / 100).toLocaleString('en-US', { style: 'currency', currency: 'USD' });
  }

  async refresh(): Promise<void> {
    try {
      const cohorts = await this.api.cohorts();
      this.cohorts.set(cohorts);
      if (!this.selectedCode() && cohorts.length > 0) this.selectedCode.set(cohorts[0].code);
      if (this.selectedCode())
        this.enrollments.set(await this.api.enrollments(this.selectedCode()));
      const id = this.logFor();
      if (id && this.auth.isStaff()) this.log.set(await this.api.events(id));
    } catch (e: unknown) {
      // A 401 means the session is gone; the interceptor has already signed the user out.
      if ((e as { status?: number }).status !== 401) this.error.set('The API is not reachable.');
      return;
    }
    if (this.error() === 'The API is not reachable.') this.error.set('');
  }

  async select(code: string): Promise<void> {
    this.selectedCode.set(code);
    this.logFor.set(null);
    this.log.set([]);
    await this.refresh();
  }

  async openCohort(): Promise<void> {
    await this.run(async () => {
      const code = this.code();
      const cohort = await this.api.openCohort({
        code,
        title: this.title(),
        track: this.track(),
        instructor: this.instructor(),
        seatCapacity: Number(this.capacity()),
      });
      if (this.code() === code) this.code.set('');
      this.selectedCode.set(cohort.code);
    });
  }

  async book(): Promise<void> {
    await this.run(async () => {
      const reference = this.reference();
      await this.api.book({
        cohortCode: this.selectedCode(),
        reference,
        seats: Number(this.seats()),
      });
      if (this.reference() === reference) this.reference.set('');
    });
  }

  async withdraw(enrollment: Enrollment): Promise<void> {
    await this.run(() => this.api.withdraw(enrollment.id));
  }

  async showLog(enrollment: Enrollment): Promise<void> {
    this.logFor.set(enrollment.id);
    this.log.set([]);
    await this.refresh();
  }

  canWithdraw(enrollment: Enrollment): boolean {
    return enrollment.status === 'Held' || enrollment.status === 'Enrolled';
  }

  private async run(action: () => Promise<unknown>): Promise<void> {
    this.error.set('');
    try {
      await action();
    } catch (e: unknown) {
      const body = (e as { error?: { title?: string } }).error;
      this.error.set(body?.title ?? 'The request failed.');
    }
    await this.refresh();
  }

  field(signalRef: { set(value: string): void }, event: Event): void {
    signalRef.set((event.target as HTMLInputElement).value);
  }
}
