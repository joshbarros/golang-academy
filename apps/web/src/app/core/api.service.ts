import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { Enrollment, LogEntry, Cohort } from './models';

export interface NewCohort {
  code: string;
  title: string;
  track: string;
  instructor: string;
  seatCapacity: number;
}

@Injectable({ providedIn: 'root' })
export class ApiService {
  private readonly http = inject(HttpClient);

  cohorts = () => firstValueFrom(this.http.get<Cohort[]>('/api/cohorts'));
  openCohort = (body: NewCohort) => firstValueFrom(this.http.post<Cohort>('/api/cohorts', body));

  enrollments = (cohortCode: string) =>
    firstValueFrom(this.http.get<Enrollment[]>('/api/enrollments', { params: { cohortCode } }));
  book = (body: { cohortCode: string; reference: string; seats: number }) =>
    firstValueFrom(this.http.post<Enrollment>('/api/enrollments', body));
  withdraw = (id: string) =>
    firstValueFrom(this.http.post<Enrollment>(`/api/enrollments/${id}/withdrawal`, {}));
  events = (id: string) =>
    firstValueFrom(this.http.get<LogEntry[]>(`/api/enrollments/${id}/events`));
}
